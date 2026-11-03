// Command chat is kingpin's own chat example, extended a little: package-level
// flags and arguments in pointer style, commands dispatched on FullCommand.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/fastly/kingpin"
)

var (
	app      = kingpin.New("chat", "A command-line chat application.")
	debug    = app.Flag("debug", "Enable debug mode.").Bool()
	noCache  = app.Flag("no-cache", "Skip the cache.").Bool()
	serverIP = app.Flag("server", "Server address.").Default("127.0.0.1").String()

	register     = app.Command("register", "Register a new user.")
	registerNick = register.Arg("nick", "Nickname for user.").Required().String()
	registerName = register.Arg("name", "Name of user.").Required().String()

	post        = app.Command("post", "Post a message to a channel.")
	postImage   = post.Flag("image", "Image to post.").String()
	postFormat  = post.Flag("format", "Message format.").Enum("plain", "markdown")
	postChannel = post.Arg("channel", "Channel to post to.").Required().String()
	postText    = post.Arg("text", "Text to post.").Strings()
	postCount   = post.Flag("count", "How many times.").Short('c').Default("1").Int()
)

func main() {
	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	// Register user
	case register.FullCommand():
		fmt.Printf("register nick=%s name=%s debug=%v no-cache=%v server=%s\n", *registerNick, *registerName, *debug, *noCache, *serverIP)

	// Post message
	case post.FullCommand():
		text := strings.Join(*postText, " ")
		fmt.Printf("post channel=%s text=%q image=%q format=%q count=%d debug=%v\n", *postChannel, text, *postImage, *postFormat, *postCount, *debug)
	}
}
