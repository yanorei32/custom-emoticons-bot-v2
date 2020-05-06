package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/go-playground/validator.v9"
	"gopkg.in/yaml.v3"

	"github.com/bwmarrin/discordgo"
	"github.com/mattn/go-colorable"
	"github.com/Sirupsen/logrus"
	bitly "github.com/zpnk/go-bitly"
)

type Configure struct {
	DiscordAPIKey	string			`validate:"required" yaml:"discord_apikey"`
	BitlyToken		string			`validate:"required" yaml:"bitly_token"`
	Quote			string			`validate:"required,len=1" yaml:"quote"`
	PingPong		bool			`yaml:"pingpong"`
	Dictionaries	[]DictionaryDef	`yaml:"dictionaries"`
}

type DictionaryDef struct {
	FileSrc				string		`validate:"required,oneof=fs http" yaml:"filesrc"`
	Path				string		`validate:"required" yaml:"path"`
	Name				string		`validate:"required" yaml:"name"`
	Timeout				int			`validate:"required,min=0" yaml:"timeout"`
	ReloadLimitSec		int			`validate:"required,min=0" yaml:"reload_limit_sec"`
	ReloadOnMessage		bool		`yaml:"reload_on_message"`
	ConcatUrlPrefix		string		`validate:"requored,oneof=discord-smart smart-custom never" yaml:"concat_url_prefix"`
	CustomUrlPrefix		string		`yaml:"custom_url_prefix"`
	ConcatUrlSuffix		string		`validate:"requored,oneof=smart-custom never" yaml:"concat_url_suffix"`
	CustomUrlSuffix		string		`yaml:"custom_url_suffix"`
	MentionProtection	bool		`yaml:"mention_protection"`
	UseBitly			string		`validate:"required,oneof=always without-gif never" yaml:"use_bitly"`
	IntervalReloadSec	int			`validate:"required,min=0" yaml:"interval_reload_sec"`
	GuildSelectionMode	string		`validate:"required,oneof=whitelist blacklist" yaml:"guild_selection_mode"`
	Guilds				[]string	`validate:"required,unique,dive,is_guild_id" yaml:"guilds"`
}

type Dictionary struct {
	Updating	sync.Mutex
	Writing		sync.Mutex
	UpdatedAt	time.Time
	Data		map[string]string
}

var dictionaries []Dictionary
var shortenCache map[string]string

const (
	discordCdnBaseURI	= "https://cdn.discordapp.com/attachments/"
	safeChars			= `[\w_\?\!]+`
)

func isGuildId(fl validator.FieldLevel) bool {
	return regexp.MustCompile(`\A\d{18}\z`).MatchString(fl.Field().String())
}

func readConf(path string) (Configure, error) {
	var c Configure

	buf, err := ioutil.ReadFile(path)
	if err != nil {
		return c, err
	}

	if err := yaml.Unmarshal(buf, &c); err != nil {
		return c, err
	}

	v := validator.New()
	v.RegisterValidation("is_guild_id", isGuildId)

	if err := v.Struct(&c); err != nil {
		return c, err
	}

	return c, nil
}

func updateDictionary(dd *DictionaryDef, d *Dictionary) error {
	d.Updating.Lock()
	defer d.Updating.Unlock()

	now			:= time.Now()
	duration	:= d.UpdatedAt.Sub(now)

	logrus.Debug("Try to update dict: " + dd.Name)

	if math.Abs(duration.Seconds()) < float64(dd.ReloadLimitSec) {
		logrus.Debug("Update rate limit.")
		return nil
	}

	var ioReader io.Reader

	if dd.FileSrc == "http" {
		httpc := &http.Client {
			Timeout: time.Duration(dd.Timeout) * time.Second,
		}

		resp, err := httpc.Get(dd.Path)
		if err != nil {
			return err
		}

		defer resp.Body.Close()

		ioReader = resp.Body

	} else if dd.FileSrc == "fs" {
		f, err := os.Open(dd.Path)
		if err != nil {
			return err
		}

		defer f.Close()

		ioReader = f

	}

	reader := csv.NewReader(ioReader)
	reader.LazyQuotes = true

	nDic := make(map[string]string)

	idRegex := regexp.MustCompile(`\A` + safeChars + `\z`)

	for {
		record, err := reader.Read()

		if err == io.EOF {
			break
		}

		if err != nil {
			return err
		}

		if len(record) < 2 {
			continue
		}

		name := record[0]
		path := record[1]

		if !idRegex.MatchString(name) {
			continue
		}

		nDic[name] = ""

		if !strings.HasPrefix(path, "http") {
			switch dd.ConcatUrlPrefix {
			case "discord-smart":
				nDic[name] += discordCdnBaseURI
			case "custom-smart":
				nDic[name] += dd.CustomUrlPrefix
			case "never":
				break
			default:
				logrus.Fatal(fmt.Sprintf(
					"ASSERT: invalid ConcatUrlPrefix passed: %s",
					dd.ConcatUrlPrefix,
				))
			}
		}

		nDic[name] += path

		if !strings.HasPrefix(path, "http") {
			switch dd.ConcatUrlSuffix {
			case "custom-smart":
				nDic[name] += dd.CustomUrlSuffix
			case "never":
				break
			default:
				logrus.Fatal(fmt.Sprintf(
					"ASSERT: invalid ConcatUrlSuffix passed: %s",
					dd.ConcatUrlSuffix,
				))
			}
		}

		if dd.MentionProtection {
			nDic[name] = strings.Replace(
				nDic[name], "@", "%40", -1,
			)
		}
	}

	d.Writing.Lock()
	defer d.Writing.Unlock()

	d.Data = nDic

	d.UpdatedAt = now

	return nil
}

func main() {
	logrus.SetOutput(colorable.NewColorableStdout())

	logrus.Debug("Read Configuration...")

	binpath, err := os.Executable()
	if err != nil {
		logrus.Fatal(err)
	}

	c, err := readConf(filepath.Join(filepath.Dir(binpath), "configure.yml"))
	if err != nil {
		logrus.Fatal(err)
	}

	bl := bitly.New(c.BitlyToken)

	dg, err := discordgo.New("Bot " + c.DiscordAPIKey)
	if err != nil {
		logrus.Fatal(err)
	}

	escapedQuote := regexp.QuoteMeta(c.Quote)

	emoticonRegex := regexp.MustCompile(
		escapedQuote + safeChars + escapedQuote,
	)

	shortenCache = make(map[string]string)

	for i, dd := range c.Dictionaries {
		dictionaries = append(dictionaries, Dictionary{})

		err := updateDictionary(&dd, &dictionaries[i])
		if err != nil {
			logrus.Error("Dictionary first loading failed: " + dd.Name)
			logrus.Fatal(err)
		}

		if dd.IntervalReloadSec != 0 {
			go func () {
				for {
					time.Sleep(
						time.Duration(dd.IntervalReloadSec) * time.Second,
					)

					err := updateDictionary(&dd, &dictionaries[i])
					if err != nil {
						logrus.Error("Dictionary update failed: " + dd.Name)
						logrus.Error(err)
					}
				}
			}()
		}
	}


	dg.AddHandler(func (s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID {
			return
		}

		if c.PingPong && m.Content == "ping" {
			s.ChannelMessageSend(m.ChannelID, "[custom-emoticons-bot] pong")
			return
		}

		g := emoticonRegex.FindSubmatch([]byte(m.Content))
		if g == nil {
			return
		}

		name := string(g[0][1:len(g[0])-1])

		for i, dd := range c.Dictionaries {
			{
				found := false

				for _, gid := range dd.Guilds {
					if m.GuildID == gid {
						found = true
						break
					}
				}

				if found && dd.GuildSelectionMode == "blacklist" {
					continue
				}

				if !found && dd.GuildSelectionMode == "whitelist" {
					continue
				}
			}

			if dd.ReloadOnMessage {
				updateDictionary(&dd, &dictionaries[i])

				if err != nil {
					logrus.Error(
						"Dictionary onmessage update failed: " + dd.Name,
					)
					logrus.Error(err)
				}
			}

			dictionaries[i].Writing.Lock()
			defer dictionaries[i].Writing.Unlock()

			url, ok := dictionaries[i].Data[name]
			if !ok {
				continue
			}

			if	dd.UseBitly == "always" ||
				dd.UseBitly != "never" &&
				url[len(url) - 3:] != "gif" {
				sUrl, ok := shortenCache[url]

				if !ok {
					l, err := bl.Links.Shorten(url)

					if err != nil {
						logrus.Printf(
							"500 bitly error %s/%s : %s\n",
							dd.Name, name, err,
						)

						continue
					}

					shortenCache[url] = l.URL
					sUrl = l.URL
				}

				url = sUrl
			}

			logrus.Printf("200 %s/%s", dd.Name, name)
			s.ChannelMessageSend(m.ChannelID, url)

			return
		}

		logrus.Printf("404 %s", name)
	})

	if err := dg.Open(); err != nil {
		logrus.Fatal("error open websocket: ", err)
	}

	// logrus.SetLevel(logrus.DebugLevel)
	logrus.Info("Bot is now running.  Press ^C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	dg.Close()
}

