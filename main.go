// (c) 2017 - Bas Westerbaan <bas@westerbaan.name>
// You may redistribute this file under the conditions of the AGPLv3.

// twittermost is a mattermost bot that posts tweets of tweeps it follows
// on twitter.  See https://github.com/bwesterb/twittermost

package main

import (
	"encoding/json"
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
	mm           *model.Client      // mattermost client
	mmu          *model.User        // mattermost user
	team         *model.Team        // mattermost team
	initialLoad  *model.InitialLoad // mattermost initial load
	channel      *model.Channel     // main channel
	debugChannel *model.Channel     // debugging channel
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
	b.mm = model.NewClient(b.conf.Url)

	// Check the connection
	if props, err := b.mm.GetPing(); err != nil {
		log.Fatalf("mattermost: could not connect: %s", err)
	} else {
		log.Printf("Connected to mattermost server %s", props["version"])
	}

	// Log in
	if loginResult, err := b.mm.Login(b.conf.User, b.conf.Password); err != nil {
		log.Fatalf("mattermost: could not login: %s", err)
	} else {
		log.Printf("mattermost: logged in as %s", b.conf.User)
		b.mmu = loginResult.Data.(*model.User)
	}

	// Initial load
	log.Println("Fetching initial data --- including teams:")
	if initialLoadResults, err := b.mm.GetInitialLoad(); err != nil {
		log.Fatalf("mattermost: GetInitialLoad() failed: %s", err)
	} else {
		b.initialLoad = initialLoadResults.Data.(*model.InitialLoad)
	}

	// Find team
	for _, team := range b.initialLoad.Teams {
		log.Printf(" - %s %s %#v", team.Id, team.Name, team.DisplayName)
		if team.Name != b.conf.Team {
			continue
		}
		b.team = team
	}

	if b.team == nil {
		log.Fatalf("Could not find team %s", b.conf.Team)
	}
	b.mm.SetTeamId(b.team.Id)

	// Join channels
	if _, err := b.mm.JoinChannelByName(b.conf.Channel); err != nil {
		log.Fatalf("Could not join channel %s: %s", b.conf.Channel, err)
	}
	if _, err := b.mm.JoinChannelByName(b.conf.DebugChannel); err != nil {
		log.Fatalf("Could not join channel %s: %s", b.conf.DebugChannel, err)
	}

	// Find channel
	log.Println("Fetching channel list:")
	if channelsResult, err := b.mm.GetChannels(""); err != nil {
		log.Fatalf("Failed to get channel list: %s", err)
	} else {
		chans := channelsResult.Data.(*model.ChannelList)
		for _, channel := range *chans {
			log.Printf(" - %s %s %#v", channel.Id, channel.Name,
				channel.DisplayName)
			if channel.Name == b.conf.Channel {
				log.Println("   (channel)")
				b.channel = channel
			}
			if channel.Name == b.conf.DebugChannel {
				log.Println("   (debugChannel)")
				b.debugChannel = channel
			}
		}
	}

	if b.channel == nil {
		log.Fatalf("Could not find channel %s", b.conf.Channel)
	}
	if b.debugChannel == nil {
		log.Fatalf("Could not find debug channel %s", b.conf.DebugChannel)
	}

	u, _ := url.Parse(b.conf.Url)
	u.Scheme = "wss" // no one should use non-SSL anyway

	if ws, err := model.NewWebSocketClient(u.String(), b.mm.AuthToken); err != nil {
		log.Fatalf("Could not connect with websocket: %s", err)
	} else {
		b.ws = ws
	}

	b.ws.Listen()
	log.Println("Listening on websockets for events ...")

	go func() {
		for event := range b.ws.EventChannel {
			if event != nil {
				b.handleWebSocketEvent(event)
			}
		}
		os.Exit(-1)
	}()

	// Say hi
	// b.Logf("I'm up ...")
}

func (b *Bot) handleWebSocketEvent(event *model.WebSocketEvent) {
	if event.Event != model.WEBSOCKET_EVENT_POSTED {
		return
	}

	post := model.PostFromJson(strings.NewReader(event.Data["post"].(string)))
	if post == nil {
		return
	}
	if post.UserId == b.mmu.Id {
		return
	}

	msg0 := strings.TrimSpace(post.Message)
	msg := strings.TrimSpace(strings.TrimPrefix(msg0, "@"+b.conf.User))
	if msg == msg0 {
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
		b.Logf("checkTimeline error: %#v", err)
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
	if _, err := b.mm.CreatePost(myPost); err != nil {
		log.Printf("postTweet failed: %s", err)
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

func (b *Bot) handleCheck(post *model.Post, args []string) {
	if !b.isTrusted(post.UserId) {
		b.replyToPost("Sorry, I don't trust you :/", post)
		return
	}
	b.checkTimeline()
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
			b.replyToPost(fmt.Sprintf("error: %#v", err), post)
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
		b.replyToPost(fmt.Sprintf("Something went wrong: %#v", err), post)
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
		b.replyToPost(fmt.Sprintf("Something went wrong: %#v", err), post)
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
		if res, err := b.mm.GetByUsername(userName, ""); err != nil {
			b.replyToPost(fmt.Sprintf("error: %#v", err.Error()), post)
			return
		} else {
			uid = res.Data.(*model.User).Id
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
		if res, err := b.mm.GetByUsername(userName, ""); err != nil {
			b.replyToPost(fmt.Sprintf("error: %#v", err.Message), post)
			return
		} else {
			uid = res.Data.(*model.User).Id
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
	if _, err := b.mm.CreatePost(myPost); err != nil {
		log.Printf("replyToPost failed: %s", err)
	}
}

func (b *Bot) setupGracefulShutdown() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for _ = range c {
			log.Printf("Interrupt received --- shutting down...")
			// b.Logf("        ... going down")
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
	post := &model.Post{
		ChannelId: b.debugChannel.Id,
		Message:   s,
	}
	if _, err := b.mm.CreatePost(post); err != nil {
		log.Printf("Failed to send debug message: %s", err)
	}
}

func main() {
	var confPath string
	conf := BotConf{
		DataPath:      "mattermost.json",
		Channel:       "town-square",
		DebugChannel:  "test",
		MaxTweets:     20,
		CheckInterval: 120,
	}

	// Parse cmdline flags
	flag.StringVar(&confPath, "config", "config.json",
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
