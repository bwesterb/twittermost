twittermost
===========

twittermost is a [Mattermost](https://about.mattermost.com) bot
that will announce the tweets of the tweeps it follows on twitter.

Installing
----------

First, install twittermost

    $ go get github.com/bwesterb/twittermost

Create a Twitter user (say @twitteruser) and a Mattermost user (say matteruser)
for the bot.  Register a [new app](https://apps.twitter.com) on twitter
to get *consumer key*, *consumer secret*, *access token* and *access secret*.

Copy `config.json.template`to `config.json` in the `conf` folder. Now edit it with the information obtained from Twitter.
Then run with `twittermost`.

Install Using Docker
------------
In case you want to run twittermost in a docker container, there is a way to do it.

Create a Twitter user (say @twitteruser) and a Mattermost user (say matteruser)
for the bot.  Register a [new app](https://apps.twitter.com) on twitter
to get *consumer key*, *consumer secret*, *access token* and *access secret*.

Then clone (or download) this project.
Copy `config.json.template`to `config.json` in the `conf` folder. Now edit it with the information obtained from Twitter.
Then run 
```
docker build . -t twittermost
docker run -it --rm --name twittermost -v "$PWD"/conf:/go/src/app/conf twittermost
```


Usage
-----

The twittermost bot will respond to command by trusted users.  To add trusted users, use:

1. `@matteruser trust username`

Like a chick which hasn't seen its mother yet, the twittermost bot will trust anyone initially, until it's told who to trust.
 
2. `@matteruser distrust username`
   Removes the given Mattermost user from the trusted users.
3. `@matteruser follow twitterhandle`
   Follow the given user on twitter.
4. `@matteruser unfollow twitterhandle`
   Unfollow the given user on twitter.
5. `@matteruser check`
   Force a twitter timeline check for updates.  
