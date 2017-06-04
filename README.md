twittermost
===========

twittermost is a [Mattermost](https://about.mattermost.com) bot
that will announce the tweets of the tweeps it follows on twitter.

Installing
----------

First, install twittermost

    $ go install https://github.com/bwesterb/twittermost

Create a Twitter user (say @twitteruser) and a Mattermost user (say matteruser)
for the bot.  Register a [new app](https://apps.twitter.com) on twitter
to get *consumer key*, *consumer secret*, *access token* and *access secret*.
Now fill a `config.js`, like this:
    
    {
        "Url":"https://domainofmattermost.com",

        "User":"matteruser",
        "Email":"email@ofmatteruser.com",
        "Password":"mattermostpassword",
        "Team":"team",
        "Channel":"channel-name-to-send-tweets-to",
        "DebugChannel":"channel-to-send-debug-messages-to",

        "ConsumerKey":"twitter consumer key",
        "ConsumerSecret":"twitter consumer secret",
        "AccessToken":"twitter access token",
        "AccessSecret":"twitter access secret"
    }

Then run with `twittermost`.  That's it!
