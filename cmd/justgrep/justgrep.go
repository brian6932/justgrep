package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Mm2PL/justgrep"
)

type progressUpdate struct {
	Type       string  `json:"type"`
	Found      int     `json:"found"`
	Channel    string  `json:"channel"`
	NextDate   string  `json:"next_date,omitempty"`
	TotalSteps float64 `json:"total_steps,omitempty"`
	LeftSteps  float64 `json:"left_steps,omitempty"`

	CurrentChannelNum int `json:"current_channel_num,omitempty"`
	CountChannels     int `json:"count_channels,omitempty"`

	Progress justgrep.ProgressState `json:"progress"`
}

type errorReport struct {
	Type     string                 `json:"type"`
	Error    string                 `json:"error"`
	Progress justgrep.ProgressState `json:"progress"`
}

type summaryReport struct {
	Type     string                 `json:"type"`
	Results  map[string]int         `json:"results"`
	Progress justgrep.ProgressState `json:"progress"`
}

type arguments struct {
	url *string

	user        *string
	notUser     *string
	userIsRegex *bool

	channel      *string
	messageRegex *string
	maxResults   *int

	msgOnly *bool

	start *string
	end   *string

	startTime time.Time
	endTime   time.Time

	verbose      *bool
	recursive    *bool
	progressJson *bool

	messageTypes    []string
	messageTypesRaw *string

	noEnv *bool
}

func parseTime(input string) (output time.Time, err error) {
	output, err = time.Parse("2006-01-02 15:04:05", input)
	if err == nil {
		return
	}
	output, err = time.Parse("2006-01-02 15:04:05-07:00", input)
	if err == nil {
		return
	}
	output, err = time.Parse(time.RFC3339, input)
	if err == nil {
		return
	}

	return time.Time{}, err
}

func (args *arguments) validateAndProcessFlags() (valid bool) {
	valid = true
	if *args.channel == "" && !*args.recursive {
		_, _ = fmt.Fprintln(os.Stderr, "You need to pass the -channel or -r (recursive) arguments.")
		valid = false
	}
	if *args.channel != "" && *args.recursive {
		_, _ = fmt.Fprintln(os.Stderr, "Passing both -r (run on all channels) and -channel does not make sense.")
		valid = false
	}
	if *args.start == "" {
		_, _ = fmt.Fprintln(os.Stderr, "You need to pass the -start argument.")
		valid = false
	}
	if *args.verbose && *args.progressJson {
		_, _ = fmt.Fprintln(os.Stderr, "Passing both -v and -progress-json doesn't make sense because they use stderr.")
		valid = false
	}
	// show missing arguments and that's it
	if !valid {
		return
	}

	startTime, err := parseTime(*args.start)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "-start: Invalid time: %s: %s\n", *args.start, err)
		valid = false
	}
	args.startTime = startTime
	if *args.end == "" {
		args.endTime = time.Now().UTC()
	} else {
		endTime, err := parseTime(*args.end)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "-end: Invalid time: %s: %s\n", *args.end, err)
			valid = false
		}
		args.endTime = endTime
	}
	return
}

const progressNextChannel = "nextChannel"
const progressNextStep = "nextStep"
const errorWhileFetching = "fetchError"
const summaryFinished = "summaryFinished"

var gitCommit = "[unavailable]"
var httpClient = http.Client{}

const EnvDefaultInstances = "JUSTGREP_DEFAULT_INSTANCES"

func main() {
	args := &arguments{}
	args.user = flag.String("user", "", "Target user")
	args.notUser = flag.String("notuser", "", "Negative match on username")
	args.userIsRegex = flag.Bool("uregex", false, "Is the -user option a regex?")

	args.msgOnly = flag.Bool(
		"msg-only",
		false,
		"Only want chat messages (PRIVMSGs). Deprecated: use -msg-types PRIVMSG",
	)
	args.messageTypesRaw = flag.String(
		"msg-types",
		"",
		"Return only messages with COMMANDs in the comma separated list.",
	)

	args.channel = flag.String("channel", "", "Target channel")
	args.messageRegex = flag.String("regex", "", "Message Regex")
	args.start = flag.String("start", "", "Start time")
	args.end = flag.String("end", "", "End time")
	args.url = flag.String("url", "", "Justlog instance URL")
	args.maxResults = flag.Int("max", 0, "How many results do you want? 0 for unlimited")

	args.verbose = flag.Bool("v", false, "Show human-readable progress information")
	args.progressJson = flag.Bool("progress-json", false, "Send JSON progress updates to stderr, not allowed with -v.")
	args.recursive = flag.Bool("r", false, "Run search on all channels.")

	args.noEnv = flag.Bool("no-env", false, "Disables reading environment variables like JUSTGREP_DEFAULT_INSTANCES")
	flag.Usage = func() {
		fmt.Fprintf(
			flag.CommandLine.Output(),
			"This is justgrep commit %s, https://github.com/Mm2PL/justgrep\n",
			gitCommit,
		)
		fmt.Fprintf(flag.CommandLine.Output(), "Basic usage:\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "Check man page for examples and longer explanations\n")
	}
	flag.Parse()
	flagsAreValid := args.validateAndProcessFlags()
	if !flagsAreValid {
		os.Exit(1)
	}

	var defaultInstancesEnv string
	defaultInstances := []string{*args.url}
	instanceListSource := "-url"

	if *args.url == "" && !*args.noEnv {
		defaultInstancesEnv = os.Getenv(EnvDefaultInstances)
		defaultInstances = strings.Split(defaultInstancesEnv, " ")
		instanceListSource = EnvDefaultInstances
	}

	if len(defaultInstances) == 1 && defaultInstances[0] == "" {
		defaultInstances = []string{"http://localhost:8025"}
		if *args.verbose {
			fmt.Fprintf(
				os.Stderr,
				"Assuming you wanted to use %s as the justlog instance. Use -url or set the %q env variable.\n",
				defaultInstances[0],
				EnvDefaultInstances,
			)
		}
	}

	if *args.recursive && len(defaultInstances) > 1 {
		instancesSafe := []string{}
		for _, instance := range defaultInstances {
			itext := ""
			u, err := url.Parse(instance)
			if err != nil {
				itext = "[failed to url parse, hiding to not show any secrets]"
			}
			itext = u.Redacted()
			instancesSafe = append(instancesSafe, itext)
		}
		fmt.Fprintf(os.Stderr, "Please provide a single -url for a search of every channel (-r).\n")
		fmt.Fprintf(
			os.Stderr,
			"Used instance list from %s:\n"+
				"- %s\n",
			instanceListSource,
			strings.Join(instancesSafe, "\n- "),
		)
		os.Exit(1)
	}

	justlogUrl := ""

	if *args.recursive {
		justlogUrl = defaultInstances[0]
	} else {
	instanceLoop:
		for _, instance := range defaultInstances {
			chns, err := justgrep.GetChannelsFromJustLog(context.Background(), &httpClient, instance)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Fetching channels from %q failed: %s\n", instance, err.Error())
				continue instanceLoop
			}
			for _, chn := range chns {
				if *args.channel == chn {
					justlogUrl = instance
					break instanceLoop
				}
			}
		}
		if justlogUrl == "" {
			fmt.Fprintf(os.Stderr, "No justlog instance has the channel %q\n", *args.channel)
			os.Exit(1)
		}
		if *args.verbose {
			fmt.Fprintf(os.Stderr, "Picked justlog: %s\n", justlogUrl)
		}
	}

	messageExpr, err := regexp.Compile(*args.messageRegex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error while compiling your message regex: %s\n", err)
		return
	}

	var userRegex *regexp.Regexp
	var negativeRegex *regexp.Regexp
	matchMode := justgrep.DontMatch
	if *args.user != "" || *args.notUser != "" {
		matchMode = justgrep.MatchExact
	}

	if *args.userIsRegex {
		matchMode = justgrep.MatchRegex
		userRegex, err = regexp.Compile(*args.user)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error while compiling your username regex: %s\n", err)
			return
		}
		negativeRegex, err = regexp.Compile(*args.notUser)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error while compiling your negative username regex: %s\n", err)
			return
		}
	}
	args.messageTypes = strings.Split(*args.messageTypesRaw, ",")
	filter := justgrep.Filter{
		StartDate: args.startTime,
		EndDate:   args.endTime,

		HasMessageType: len(*args.messageTypesRaw) != 0,
		MessageTypes:   args.messageTypes,

		HasMessageRegex: true,
		MessageRegex:    messageExpr,

		UserMatchType: matchMode,

		UserName:         strings.ToLower(*args.user),
		NegativeUserName: strings.ToLower(*args.notUser),

		NegativeUserRegex: negativeRegex,
		UserRegex:         userRegex,

		Count: *args.maxResults,
	}
	var channelsToSearch []string
	if !*args.recursive {
		channelsToSearch = strings.Split(*args.channel, ",")
	} else {
		channelsToSearch, err = justgrep.GetChannelsFromJustLog(context.Background(), &httpClient, justlogUrl)
		if err != nil {
			_, err := fmt.Fprintf(os.Stderr, "Error while fetching channels from justlog: %s", err)
			if err != nil {
				return
			}
			os.Exit(1)
		}
	}
	// fix name changes and USERNOTICEs not showing up when using per-user log endpoint
	if *args.user != "" && !(*args.userIsRegex) {
		filter.UserMatchType = justgrep.DontMatch
	}

	progress := &justgrep.ProgressState{
		TotalResults: make([]int, justgrep.ResultCount),
		BeginTime:    time.Now(),
	}
	for currentIndex, channel := range channelsToSearch {
		if *args.verbose {
			_, _ = fmt.Fprintf(os.Stderr, "Now scanning #%s %d/%d\n", channel, currentIndex+1, len(channelsToSearch))
		}
		if *args.progressJson {
			_ = json.NewEncoder(os.Stderr).Encode(
				progressUpdate{
					Type:              progressNextChannel,
					Found:             progress.TotalResults[justgrep.ResultOk],
					Channel:           channel,
					CurrentChannelNum: currentIndex,
					CountChannels:     len(channelsToSearch),
					Progress:          *progress,
				},
			)
		}
		var api justgrep.JustlogAPI
		if *args.user != "" && !(*args.userIsRegex) {
			api = &justgrep.UserJustlogAPI{User: *args.user, Channel: channel, URL: justlogUrl}
		} else {
			api = &justgrep.ChannelJustlogAPI{Channel: channel, URL: justlogUrl}
		}
		searchLogs(args, api, filter, progress)
	}
	if *args.verbose {
		_, _ = fmt.Fprintf(os.Stderr, "Summary:\n")
		if progress.CountLines == 0 {
			// no lines fetched at all
			fmt.Fprintf(os.Stderr, "Nothing here. No lines were processed.\n")
			return
		}
		for result, count := range progress.TotalResults {
			_, _ = fmt.Fprintf(os.Stderr, " - %s => %d\n", justgrep.FilterResult(result), count)
		}
		const Mega = 1000.0 * 1000.0
		const Milli = 0.001
		timeTaken := time.Now().Sub(progress.BeginTime)
		_, _ = fmt.Fprintf(
			os.Stderr,
			"Processed %.2f MB (%.2f MB/s)\n"+
				"Lines processed: %d\n"+
				"Average line length: %d\n"+
				"Time taken: %s\n",
			float64(progress.CountBytes)/Mega,
			float64(progress.CountBytes)/float64(timeTaken.Milliseconds())/Milli/Mega,
			progress.CountLines,
			progress.CountBytes/progress.CountLines,
			timeTaken.Truncate(time.Second),
		)
	}
	if *args.progressJson {
		res := make(map[string]int)
		for result, count := range progress.TotalResults {
			res[justgrep.FilterResult(result).String()] = count
		}
		_ = json.NewEncoder(os.Stderr).Encode(
			summaryReport{
				Type:     summaryFinished,
				Results:  res,
				Progress: *progress,
			},
		)
	}
}

const progressSize = 50

func makeProgressBar(totalSteps float64, stepsLeft float64) string {
	var fracDone float64
	if totalSteps == 0 {
		fracDone = 0
		stepsLeft = 1
		totalSteps = 2
	} else {
		fracDone = 1 - stepsLeft/totalSteps
	}
	done := strings.Repeat("=", int(math.Floor(progressSize*fracDone)))
	left := strings.Repeat(" ", int(math.Ceil(progressSize*(1-fracDone))))
	return fmt.Sprintf("[%s>%s] %.2f%%", done, left, fracDone*100)
}

func searchLogs(
	args *arguments,
	api justgrep.JustlogAPI,
	filter justgrep.Filter,
	progress *justgrep.ProgressState,
) {
	nextDate := args.endTime
	ctx, cancel := context.WithCancel(context.Background())
	var channel string
	step := api.GetApproximateOffset()
	switch api.(type) {
	default:
		channel = fmt.Sprintf("[unknown] (%t)", api)
		step = time.Hour * 24
	case *justgrep.UserJustlogAPI:
		channel = api.(*justgrep.UserJustlogAPI).Channel
	case *justgrep.ChannelJustlogAPI:
		channel = api.(*justgrep.ChannelJustlogAPI).Channel
	}
	totalSteps := float64(args.endTime.Sub(args.startTime) / step)

	defer cancel()
	for {
		stepsLeft := float64(nextDate.Sub(args.startTime) / step)
		if *args.verbose {
			nowTime := time.Now()
			timeTaken := float64(nowTime.Sub(progress.BeginTime) / time.Second)
			if timeTaken == 0 {
				timeTaken = 1
			}
			_, _ = fmt.Fprintf(
				os.Stderr,
				"Found %d matching messages... Downloading #%s at %s %s. %d/s (%.2f MB/s before compression). "+
					"Processed %.2f MB (%d lines and counting)\n",
				progress.TotalResults[justgrep.ResultOk],
				channel,
				nextDate.Format("2006-01-02"),
				makeProgressBar(totalSteps, stepsLeft),
				progress.CountLines/int(timeTaken),
				float64(progress.CountBytes/1000/1000)/timeTaken,

				float64(progress.CountBytes/1000/1000),
				progress.CountLines,
			)
		}
		if *args.progressJson {
			_ = json.NewEncoder(os.Stderr).Encode(
				progressUpdate{
					Type:       progressNextStep,
					Found:      progress.TotalResults[justgrep.ResultOk],
					Channel:    channel,
					NextDate:   nextDate.Format(time.RFC3339),
					TotalSteps: totalSteps,
					LeftSteps:  stepsLeft,
					Progress:   *progress,
				},
			)
		}
		download := make(chan *justgrep.Message)
		var err error
		nextDate, err = justgrep.FetchForDate(ctx, api, nextDate, download, progress, &httpClient)
		if err != nil {
			if *args.progressJson {
				_ = json.NewEncoder(os.Stderr).Encode(
					errorReport{
						Type:     errorWhileFetching,
						Error:    err.Error(),
						Progress: *progress,
					},
				)
			} else {
				_, _ = fmt.Fprintf(os.Stderr, "Error while fetching logs: %s\n", err)
			}
			break
		}

		filtered := make(chan *justgrep.Message)
		var results []int
		go func() {
			results = filter.StreamFilter(cancel, download, filtered, progress)
		}()
		for msg := range filtered {
			fmt.Println(msg.Raw)
		}

		for result, count := range results {
			progress.TotalResults[result] += count
		}
		if results[justgrep.ResultDateBeforeStart] != 0 || results[justgrep.ResultMaxCountReached] != 0 {
			break
		}
	}
}
