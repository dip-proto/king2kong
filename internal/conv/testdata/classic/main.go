// Command classic uses kingpin v2's package-level application the way its
// README does: flags, commands and arguments declared in pointer style, a
// default command, aliases, hidden flags, placeholders and the compact usage
// template.
package main

import (
	"fmt"

	"github.com/alecthomas/kingpin/v2"
)

var (
	debug   = kingpin.Flag("debug", "Enable debug mode.").Bool()
	timeout = kingpin.Flag("timeout", "Timeout waiting for ping.").Default("5s").Envar("PING_TIMEOUT").Short('t').Duration()
	secret  = kingpin.Flag("secret", "Hidden flag.").Hidden().String()

	ping      = kingpin.Command("ping", "Ping a host.").Default()
	pingIP    = ping.Arg("ip", "IP address to ping.").Required().IP()
	pingCount = ping.Arg("count", "Number of packets to send.").Int()

	post        = kingpin.Command("post", "Post a message.").Alias("send")
	postServer  = post.Flag("server", "Server to post to.").PlaceHolder("HOST:PORT").Default("localhost:80").String()
	postLevel   = post.Flag("level", "Message level.").Default("info").Enum("debug", "info", "warn")
	postRatio   = post.Flag("ratio", "Sampling ratio.").Default("0.5").Float64()
	postTags    = post.Flag("tag", "Tags.").Short('T').Strings()
	postMessage = post.Arg("message", "Message.").Required().Strings()
)

func main() {
	kingpin.UsageTemplate(kingpin.CompactUsageTemplate).Version("0.1.0").Author("Someone")
	kingpin.CommandLine.Help = "An example of kingpin's package-level API."
	kingpin.CommandLine.HelpFlag.Short('h')
	switch kingpin.Parse() {
	case ping.FullCommand():
		fmt.Printf("ping %s count=%d debug=%v timeout=%s secret=%q\n", *pingIP, *pingCount, *debug, *timeout, *secret)
	case post.FullCommand():
		fmt.Printf("post %v server=%s level=%s ratio=%g tags=%v\n", *postMessage, *postServer, *postLevel, *postRatio, *postTags)
	}
}
