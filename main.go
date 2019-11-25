// (c) 2017 - Bas Westerbaan <bas@westerbaan.name>
// You may redistribute this file under the conditions of the AGPLv3.

// twittermost is a mattermost bot that posts tweets of tweeps it follows
// on twitter.  See https://github.com/bwesterb/twittermost

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/dghubble/go-twitter/twitter"
	"github.com/dghubble/oauth1"
	"github.com/mattermost/mattermost-server/model"
)

type botData struct {
	Trusted  map[string]bool // userId -> bool map of trusted users
	LastPost int64           // id of last read twitter id
}

type BotConf struct {
	Url      string // URL to mattermost instance
	DataPath string // path to data

	// mattermost settings
	User         string
	Email        string
	Password     string
	Token        string
	Team         string
	Channel      string
	DebugChannel string

	// twitter settings
	ConsumerKey    string
	ConsumerSecret string
	AccessToken    string
	AccessSecret   string
	MaxTweets      int
	CheckInterval  int
}

type Bot struct {
	commandHandlers map[string]commandHandler
	conf            BotConf
	data            botData
	dataLock        sync.Mutex
	running         bool

	// mattermost
	mm           *model.Client4 // mattermost client
	mmu          *model.User    // mattermost user
	team         *model.Team    // mattermost team
	channel      *model.Channel // main channel
	debugChannel *model.Channel // debugging channel
	ws           *model.WebSocketClient

	// twitter
	tw          *twitter.Client
	twu         *twitter.User
	checkTicker *time.Ticker
}

type commandHandler func(*model.Post, []string)

func NewBot(conf BotConf) (b *Bot) {
	b = &Bot{conf: conf}
	b.commandHandlers = map[string]commandHandler{
		"ping":      b.handlePing,
		"follow":    b.handleFollow,
		"unfollow":  b.handleUnfollow,
		"followers": b.handleFollowers,
		"trust":     b.handleTrust,
		"distrust":  b.handleDistrust,
		"check":     b.handleCheck,
		"clear":     b.handleClear,
	}
	return
}

func (b *Bot) handleUnknownCommand(post *model.Post, args []string) {
	cmds := ""
	for k := range b.commandHandlers {
		if cmds == "" {
			cmds = k
		} else {
			cmds += ", " + k
		}
	}
	b.replyToPost("Sorry, I don't understand that command.  "+
		"Available commands: "+cmds, post)
}

// sets up the mattermost connection
func (b *Bot) setupMattermost() {
	b.mm = model.NewAPIv4Client(b.conf.Url)

	// Check the connection
	if _, response := b.mm.GetPing(); response.Error != nil {
		log.Fatalf("mattermost: could not connect: %s", response.Error)
	} else {
		log.Printf("Connected to mattermost server at %s", b.conf.Url)
	}

	// Log in
	if b.conf.Token != "" {
		b.mm.SetOAuthToken(b.conf.Token)
		if user, result := b.mm.GetMe(""); result.Error != nil {
			log.Fatalf("mattermost: could not login: %s", result.Error)
		} else {
			log.Printf("mattermost: logged in as %s", user.Username)
			b.mmu = user
		}
	} else {
		if user, result := b.mm.Login(b.conf.User, b.conf.Password); result.Error != nil {
			log.Fatalf("mattermost: could not login: %s", result.Error)
		} else {
			log.Printf("mattermost: logged in as %s", user.Username)
			b.mmu = user
		}
	}

	// Find team
	if team, result := b.mm.GetTeamByName(b.conf.Team, ""); result.Error != nil {
		log.Fatalf("Could not find team %s: %s", b.conf.Team, result.Error)
	} else {
		b.team = team
	}

	// Find DebugChannel
	if b.conf.DebugChannel != "" {
		if channel, result := b.mm.GetChannelByName(
			b.conf.DebugChannel,
			b.team.Id,
			""); result.Error != nil {
			log.Fatalf("Could not find debug channel %s", b.conf.DebugChannel)
		} else {
			b.debugChannel = channel
		}
	} else {
		log.Println("No DebugChannel set --- printing to stdout instead")
	}

	// Find Channel
	if channel, result := b.mm.GetChannelByName(b.conf.Channel, b.team.Id, ""); result.Error != nil {
		log.Fatalf("Could not find channel %s", b.conf.Channel)
	} else {
		b.channel = channel
	}

	// Join channels
	if _, result := b.mm.AddChannelMember(b.channel.Id, b.mmu.Id); result.Error != nil {
		log.Fatalf("Could not join channel %s: %s", b.conf.Channel, result.Error)
	}

	if b.debugChannel != nil {
		if _, result := b.mm.AddChannelMember(b.debugChannel.Id, b.mmu.Id); result.Error != nil {
			log.Fatalf("Could not join channel %s: %s", b.conf.Channel, result.Error)
		}
	}

	go func() {
		b.setupWebSocketClient()

		for {
			for event := range b.ws.EventChannel {
				if event != nil {
					b.handleWebSocketEvent(event)
				}
			}
			if !b.running {
				return
			}
			log.Println("Websockets connection lost.")

			bo := backoff.NewExponentialBackOff()
			err := backoff.Retry(b.setupWebSocketClient, bo)
			if err != nil {
				log.Fatalf("Failed to reconnect websockets: %s", err)
			}
		}
	}()
}

func (b *Bot) setupWebSocketClient() error {
	log.Println("Connecting websocket to listen for events ...")
	u, _ := url.Parse(b.conf.Url)
	u.Scheme = "wss" // no one should use non-SSL anyway

	if ws, err := model.NewWebSocketClient4(u.String(), b.mm.AuthToken); err != nil {
		log.Printf("  failed: %s", err)
		return err
	} else {
		b.ws = ws
	}

	b.ws.Listen()

	// Handle first message to check that the connection has been properly setup
	ok := false
	for event := range b.ws.EventChannel {
		if event != nil {
			b.handleWebSocketEvent(event)
		}
		ok = true
		break
	}
	if !ok {
		return errors.New("Failed to read message")
	}

	log.Println("  done!")
	return nil
}

func (b *Bot) handleWebSocketEvent(event *model.WebSocketEvent) {
	if event.Event != model.WEBSOCKET_EVENT_POSTED {
		return
	}

	isDM := event.Data["channel_type"] == "D"
	post := model.PostFromJson(strings.NewReader(event.Data["post"].(string)))
	if post == nil {
		return
	}
	if post.UserId == b.mmu.Id {
		return
	}

	msg0 := strings.TrimSpace(post.Message)
	msg := strings.TrimSpace(strings.TrimPrefix(msg0, "@"+b.conf.User))
	if !isDM && msg == msg0 {
		return // message does not start with @ourusername
	}

	bits := strings.SplitN(msg, " ", 2)
	cmd := bits[0]

	handler, ok := b.commandHandlers[cmd]
	if !ok {
		handler = b.handleUnknownCommand
	}
	handler(post, bits[1:])
}

func (b *Bot) checkTimeline() {
	pars := twitter.HomeTimelineParams{
		Count:   b.conf.MaxTweets,
		SinceID: b.data.LastPost,
	}
	tweets, _, err := b.tw.Timelines.HomeTimeline(&pars)
	if err != nil {
		b.Logf("checkTimeline error: %s", err)
		return
	}

	for _, tweet := range tweets {
		if tweet.ID > b.data.LastPost {
			b.data.LastPost = tweet.ID
		}
	}
	b.saveData()

	for _, tweet := range tweets {
		b.postTweet(tweet)
	}
}

func (b *Bot) postTweet(tweet twitter.Tweet) {
	// TODO quoted tweets?
	var text string = fmt.Sprintf(
		"@[%s](https://twitter.com/%s) [tweeted](https://twitter.com/statuses/%d)",
		tweet.User.ScreenName, tweet.User.ScreenName, tweet.ID)
	if tweet.Retweeted {
		text += fmt.Sprintf(" RT @[%s](https://twitter.com/%s)\n> %s",
			tweet.RetweetedStatus.User.ScreenName,
			tweet.RetweetedStatus.User.ScreenName,
			tweet.RetweetedStatus.Text)
	} else {
		text += "\n> " + tweet.Text
	}
	myPost := &model.Post{
		ChannelId: b.channel.Id,
		Message:   text,
	}
	if _, result := b.mm.CreatePost(myPost); result.Error != nil {
		log.Printf("postTweet failed: %s", result.Error)
	}
}

// Check if the given user is trusted
func (b *Bot) isTrusted(userId string) bool {
	if len(b.data.Trusted) == 0 {
		return true
	}
	trusted, ok := b.data.Trusted[userId]
	return ok && trusted
}

func (b *Bot) handlePing(post *model.Post, args []string) {
	b.replyToPost("pong", post)
}

func (b *Bot) handleClear(post *model.Post, args []string) {
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}

	if b.debugChannel == nil {
		b.replyToPost("No DebugChannel set: there is nothing to clear!", post)
		return
	}

	pageSize := 1000
	page := 0

	for {
		list, res := b.mm.GetPostsForChannel(b.debugChannel.Id, page, pageSize, "")
		if res.Error != nil {
			b.replyToPost(fmt.Sprintf("error: %s", res.Error), post)
			return
		}

		for _, post := range list.Posts {
			if post.UserId != b.mmu.Id {
				continue
			}
			if len(post.ParentId) > 0 {
				continue
			}
			if ok, res := b.mm.DeletePost(post.Id); !ok {
				b.replyToPost(fmt.Sprintf("error: %s", res.Error), post)
				return
			}
		}

		if len(list.Posts) == 0 {
			break
		}
		page++
	}

	b.replyToPost("done!", post)
}

func (b *Bot) handleCheck(post *model.Post, args []string) {
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}
	b.checkTimeline()
	b.replyToPost("done!", post)
}

func (b *Bot) handleFollowers(post *model.Post, args []string) {
	// Blocks on https://github.com/dghubble/go-twitter/issues/72
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}

	var friends []string = []string{}
	var cursor int64
	for {
		pars := twitter.FriendListParams{
			Cursor:              cursor,
			IncludeUserEntities: new(bool),
		}
		*pars.IncludeUserEntities = true
		if resp, _, err := b.tw.Friends.List(&pars); err != nil {
			b.replyToPost(fmt.Sprintf("error: %s", err), post)
			return
		} else {
			if len(resp.Users) == 0 {
				break
			}
			for _, u := range resp.Users {
				friends = append(friends, u.ScreenName)
			}
			cursor = resp.NextCursor
		}
		break
	}

	b.replyToPost(fmt.Sprintf("I'm following: %#v", friends), post)
}

func (b *Bot) handleUnfollow(post *model.Post, arg []string) {
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}

	handle := strings.TrimPrefix(strings.TrimSpace(arg[0]), "@")
	pars := twitter.FriendshipDestroyParams{ScreenName: handle}
	if _, _, err := b.tw.Friendships.Destroy(&pars); err != nil {
		b.replyToPost(fmt.Sprintf("Something went wrong: %s", err), post)
		return
	}

	b.replyToPost("Ok!", post)
}

func (b *Bot) handleFollow(post *model.Post, arg []string) {
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}

	handle := strings.TrimPrefix(strings.TrimSpace(arg[0]), "@")
	pars := twitter.FriendshipCreateParams{ScreenName: handle}
	if _, _, err := b.tw.Friendships.Create(&pars); err != nil {
		b.replyToPost(fmt.Sprintf("Something went wrong: %s", err), post)
		return
	}

	b.replyToPost("Ok!", post)
}

func (b *Bot) handleTrust(post *model.Post, arg []string) {
	var uid string
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}
	if len(arg) == 0 {
		b.replyToPost("Missing argument", post)
		return
	}
	if arg[0] == "me" {
		uid = post.UserId
	} else {
		userName := strings.TrimPrefix(strings.TrimSpace(arg[0]), "@")
		if res, result := b.mm.GetUserByUsername(userName, ""); result.Error != nil {
			b.replyToPost(fmt.Sprintf("error: %s", result.Error), post)
			return
		} else {
			uid = res.Id
		}
	}

	old, ok := b.data.Trusted[uid]
	if ok && old {
		b.replyToPost("already trusted", post)
		return
	}

	b.data.Trusted[uid] = true
	b.saveData()
	b.replyToPost("Ok!", post)
}

func (b *Bot) handleDistrust(post *model.Post, arg []string) {
	var uid string
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}
	if len(arg) == 0 {
		b.replyToPost("Missing argument", post)
		return
	}
	if arg[0] == "me" {
		uid = post.UserId
	} else {
		userName := strings.TrimPrefix(strings.TrimSpace(arg[0]), "@")
		if res, result := b.mm.GetUserByUsername(userName, ""); result.Error != nil {
			b.replyToPost(fmt.Sprintf("error: %s", result.Error), post)
			return
		} else {
			uid = res.Id
		}
	}

	old, ok := b.data.Trusted[uid]
	if ok && !old {
		b.replyToPost("already distrusted", post)
		return
	}

	b.data.Trusted[uid] = false
	b.saveData()
	b.replyToPost("Ok!", post)
}

func (b *Bot) replyToPost(msg string, post *model.Post) {
	log.Println("Sending message")
	myPost := &model.Post{
		ChannelId: post.ChannelId,
		Message:   msg,
		RootId:    post.Id,
	}
	if _, result := b.mm.CreatePost(myPost); result.Error != nil {
		log.Printf("replyToPost failed: %s", result.Error)
	}
}

func (b *Bot) setupGracefulShutdown() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for _ = range c {
			log.Printf("Interrupt received --- shutting down...")
			b.running = false
			if b.ws != nil {
				log.Println("  websockets")
				b.ws.Close()
			}
			if b.checkTicker != nil {
				log.Println("  ticker")
				b.checkTicker.Stop()
			}
			log.Println("  data")
			b.saveData()
			log.Println("     ... done:  bye!")
			os.Exit(0)
		}
	}()
}

func (b *Bot) setupTimelineCheck() {
	b.checkTicker = time.NewTicker(time.Second * time.Duration(b.conf.CheckInterval))
	go func() {
		for _ = range b.checkTicker.C {
			b.checkTimeline()
		}
	}()
}

func (b *Bot) Run() {
	// Set up mattermost client
	if b.running {
		panic("already running")
	}
	b.running = true

	b.setupGracefulShutdown()
	b.loadData()
	b.setupTwitter()
	b.setupMattermost()
	b.setupTimelineCheck()
}

func (b *Bot) loadData() {
	b.dataLock.Lock()
	defer b.dataLock.Unlock()
	buf, err := ioutil.ReadFile(b.conf.DataPath)
	if os.IsNotExist(err) {
		b.data.Trusted = make(map[string]bool)
		return
	} else if err != nil {
		log.Fatalf("Could not load data file %s: %s", b.conf.DataPath, err)
	}

	if err := json.Unmarshal(buf, &b.data); err != nil {
		log.Fatalf("Could not parse data file: %s", err)
	}

	// This might happen if a user edits twittermost.json manually.
	// See issue #12.
	if b.data.Trusted == nil {
		b.data.Trusted = make(map[string]bool)
	}
}

func (b *Bot) saveData() {
	b.dataLock.Lock()
	defer b.dataLock.Unlock()
	buf, _ := json.Marshal(&b.data)
	if err := ioutil.WriteFile(b.conf.DataPath, buf, 0600); err != nil {
		log.Fatalf("Could not write data file %s: %s", b.conf.DataPath, err)
	}
}

func (b *Bot) setupTwitter() {
	token := oauth1.NewToken(b.conf.AccessToken, b.conf.AccessSecret)
	conf := oauth1.NewConfig(b.conf.ConsumerKey, b.conf.ConsumerSecret)
	b.tw = twitter.NewClient(conf.Client(oauth1.NoContext, token))
	verifyParams := &twitter.AccountVerifyParams{}

	// logging in
	if twu, _, err := b.tw.Accounts.VerifyCredentials(verifyParams); err != nil {
		log.Fatalf("twitter: failed to login: %s", err)
	} else {
		b.twu = twu
	}
	log.Printf("twitter: logged in as @%s", b.twu.ScreenName)
}

func (b *Bot) Logf(msg string, args ...interface{}) {
	s := fmt.Sprintf(msg, args...)
	if b.debugChannel == nil {
		log.Printf("DebugChannel: %s", s)
		return
	}
	post := &model.Post{
		ChannelId: b.debugChannel.Id,
		Message:   s,
	}
	if _, result := b.mm.CreatePost(post); result.Error != nil {
		log.Printf("Failed to send debug message: %s", result.Error)
	}
}

func main() {
	var confPath = "conf/config.json"
	conf := BotConf{
		DataPath:      "mattermost.json",
		Channel:       "town-square",
		DebugChannel:  "test",
		MaxTweets:     20,
		CheckInterval: 120,
	}

	//Check whether old config file exists
	if _, err := os.Stat("config.json"); !os.IsNotExist(err) {
		// fall back to old config file location
		confPath = "config.json"
	}

	// Parse cmdline flags
	flag.StringVar(&confPath, "config", confPath,
		"Path to configuration file")
	flag.Parse()

	// Read config file
	buf, err := ioutil.ReadFile(confPath)
	if err != nil {
		log.Fatalf("Could not read config file %s: %s", confPath, err)
	}

	if err := json.Unmarshal(buf, &conf); err != nil {
		log.Fatalf("Could not parse config file: %s", err)
	}

	bot := NewBot(conf)
	bot.Run()
	select {}
}
