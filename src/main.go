package main

import (
	"encoding/csv"
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
	CsvUrl				string		`validate:"required,url" yaml:"csv_url"`
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
	Processed	map[string]string
}

var dictionaries []Dictionary

var shortenUrlCache map[string]string

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

	httpc := &http.Client {
		Timeout: time.Duration(dd.Timeout) * time.Second,
	}

	resp, err := httpc.Get(dd.CsvUrl)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	reader := csv.NewReader(resp.Body)
	reader.LazyQuotes = true

	newD := make(map[string]string)

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

		if !idRegex.MatchString(record[0]) {
			continue
		}

		newD[record[0]] = ""

		if !strings.HasPrefix(record[1], "http") {
			switch dd.ConcatUrlPrefix {
			case "discord-smart":
				newD[record[0]] += discordCdnBaseURI
			case "custom-smart":
				newD[record[0]] += dd.CustomUrlPrefix
			}
		}

		newD[record[0]] += record[1]

		if !strings.HasPrefix(record[1], "http") {
			switch dd.ConcatUrlSuffix {
			case "custom-smart":
				newD[record[0]] += dd.CustomUrlSuffix
			}
		}

		if dd.MentionProtection {
			newD[record[0]] = strings.Replace(
				newD[record[0]],
				"@",
				"%40",
				-1,
			)
		}

		if _, ok := d.Data[record[0]]; ok {
			if newD[record[0]] != d.Data[record[0]] {
				logrus.Debug("Delete old cache")
				delete(d.Processed, record[0])
			}
		}
	}

	d.Writing.Lock()
	defer d.Writing.Unlock()

	d.Data = newD

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

	c, err := readConf(filepath.Join(
		filepath.Dir(binpath),
		"configure.yml",
	))

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

	logrus.Info(emoticonRegex)
	logrus.Info(bl)

	for i, dd := range c.Dictionaries {
		dictionaries = append(dictionaries, Dictionary{})

		updateDictionary(&dd, &dictionaries[i])
		dictionaries[i].Processed = make(map[string]string)

		if dd.IntervalReloadSec != 0 {
			go func () {
				for {
					time.Sleep(
						time.Duration(dd.IntervalReloadSec) * time.Second,
					)

					updateDictionary(&dd, &dictionaries[i])
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

		groups := emoticonRegex.FindSubmatch([]byte(m.Content))
		if groups == nil {
			return
		}

		emoticonName := string(groups[0][1:len(groups[0])-1])

		for i, dd := range c.Dictionaries {
			isFound := false

			for _, gid := range dd.Guilds {
				if m.GuildID == gid {
					isFound = true
					break
				}
			}

			if isFound && dd.GuildSelectionMode == "blacklist" {
				continue
			}

			if !isFound && dd.GuildSelectionMode == "whitelist" {
				continue
			}

			if dd.ReloadOnMessage {
				updateDictionary(&dd, &dictionaries[i])
			}

			dictionaries[i].Writing.Lock()
			defer dictionaries[i].Writing.Unlock()

			_, ok := dictionaries[i].Processed[emoticonName]

			if !ok {
				original, ok := dictionaries[i].Data[emoticonName]

				if !ok {
					continue
				}

				if	dd.UseBitly != "always" && (
						dd.UseBitly == "never" ||
						original[len(original) - 3:] == "gif" ) {

					dictionaries[i].Processed[emoticonName] = original

					goto POST_EMOTE
				}

				linkObj, err := bl.Links.Shorten(
					original,
				)

				if err != nil {
					logrus.Printf(
						"500 bitly error %s/%s : %s\n",
						dd.Name,
						emoticonName,
						err,
					)

					continue
				}

				dictionaries[i].Processed[emoticonName] = linkObj.URL
			}

			POST_EMOTE:

			logrus.Printf(
				"200 %s/%s",
				dd.Name,
				emoticonName,
			)

			s.ChannelMessageSend(
				m.ChannelID,
				dictionaries[i].Processed[emoticonName],
			)

			return
		}

		logrus.Printf("404 %s", emoticonName)
	})

	if err := dg.Open(); err != nil {
		logrus.Fatal("error open websocket: ", err)
	}

	logrus.SetLevel(logrus.DebugLevel)
	logrus.Info("Bot is now running.  Press ^C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	dg.Close()
}

