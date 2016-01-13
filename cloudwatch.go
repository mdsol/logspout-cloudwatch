package cloudwatch

import (
	"bytes"
	"log"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

func init() {
	router.AdapterFactories.Register(NewCloudwatchAdapter, "cloudwatch")
}

// CloudwatchAdapter is an adapter that streams JSON to AWS CloudwatchLogs.
// It mostly just checkes ENV vars and other container info to determine
// the LogGroup and LogStream for each message, then sends each message
// on to a CloudwatchBatcher, which batches messages for upload to AWS.
type CloudwatchAdapter struct {
	route   *router.Route
	client  *docker.Client
	osHost  string
	batcher *CloudwatchBatcher // batches up messages by log group and stream
}

// NewCloudwatchAdapter creates a CloudwatchAdapter for the current region.
func NewCloudwatchAdapter(route *router.Route) (router.LogAdapter, error) {
	dockerHost := `unix:///var/run/docker.sock`
	if envVal := os.Getenv(`DOCKER_HOST`); envVal != "" {
		dockerHost = envVal
	}
	client, err := docker.NewClient(dockerHost)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	return &CloudwatchAdapter{
		route:   route,
		client:  client,
		osHost:  hostname,
		batcher: NewCloudwatchBatcher(route),
	}, nil
}

// Stream implements the router.LogAdapter interface.
func (a *CloudwatchAdapter) Stream(logstream chan *router.Message) {
	for m := range logstream {
		// determine the log group name and log stream name

		// make a render context with the required info
		containerData, err := a.client.InspectContainer(m.Container.ID)
		if err != nil {
			log.Println("cloudwatch - error inspecting container:", err)
			continue
		}
		context := RenderContext{
			Env:        parseEnv(m.Container.Config.Env),
			Labels:     containerData.Config.Labels,
			Name:       strings.TrimPrefix(m.Container.Name, `/`),
			ID:         m.Container.ID,
			Host:       m.Container.Config.Hostname,
			LoggerHost: a.osHost,
		}

		groupName := a.renderEnvValue(`LOG_GROUP`, &context, a.osHost)
		streamName := a.renderEnvValue(`LOG_STREAM`, &context, context.Name)
		// TODO - cache these two values, and index them by container ID
		// so that all the above logic happens per container, not per message!

		a.batcher.Input <- CloudwatchMessage{
			Message:   m.Data,
			Group:     groupName,
			Stream:    streamName,
			Time:      time.Now(),
			Container: context.Name,
		}
	}
}

type RenderContext struct {
	Host       string            // container host name
	Env        map[string]string // container ENV
	Labels     map[string]string // container Labels
	Name       string            // container Name
	ID         string            // container ID
	LoggerHost string            // hostname of logging container (os.Hostname)
}

// HELPER FUNCTIONS

// Searches the OS environment, then the route options, then the render context
// Env for a given key, then uses the value (or the provided default value)
// as template text, which is then rendered in the given context.
// The rendered result is returned - or the default value on any errors.
func (a *CloudwatchAdapter) renderEnvValue(
	envKey string, context *RenderContext, defaultVal string) string {
	finalVal := defaultVal
	if logspoutEnvVal := os.Getenv(envKey); logspoutEnvVal != "" {
		finalVal = logspoutEnvVal // use $envKey, if set
	}
	if routeOptionsVal, exists := a.route.Options[envKey]; exists {
		finalVal = routeOptionsVal
	}
	if containerEnvVal, exists := context.Env[envKey]; exists {
		finalVal = containerEnvVal // or, $envKey from container!
	}
	template, err := template.New("template").Parse(finalVal)
	if err != nil {
		log.Println("cloudwatch: error parsing template", finalVal, ":", err)
		return defaultVal
	} else { // render the templates in the generated context
		var renderedValue bytes.Buffer
		err = template.Execute(&renderedValue, context)
		if err != nil {
			log.Printf("cloudwatch: error rendering template %s : %s\n",
				finalVal, err)
			return defaultVal
			finalVal = renderedValue.String()
		}
	}
	return finalVal
}

func parseEnv(envLines []string) map[string]string {
	env := map[string]string{}
	for _, line := range envLines {
		fields := strings.Split(line, `=`)
		if len(fields) > 1 {
			env[fields[0]] = strings.Join(fields[1:], `=`)
		}
	}
	return env
}

// MESSAGE AND MESSAGE BATCH DEFINITION

// CloudwatchMessage is a simple JSON input to Cloudwatch.
type CloudwatchMessage struct {
	Message   string    `json:"message"`
	Group     string    `json:"group"`
	Stream    string    `json:"stream"`
	Time      time.Time `json:"time"`
	Container string    `json:"container"`
}

type CloudwatchBatch struct {
	Msgs []CloudwatchMessage
	Size int64
}

// Rules for creating Cloudwatch Log batches, from https://goo.gl/TrIN8c
const MAX_BATCH_SIZE = 3000 // 1048576 // bytes
const MSG_OVERHEAD = 26     // bytes

func msgSize(msg CloudwatchMessage) int64 {
	return int64((len(msg.Message) * 8) + MSG_OVERHEAD)
}

func NewCloudwatchBatch() *CloudwatchBatch {
	return &CloudwatchBatch{
		Msgs: []CloudwatchMessage{},
		Size: 0,
	}
}

func (b *CloudwatchBatch) Append(msg CloudwatchMessage) {
	b.Msgs = append(b.Msgs, msg)
	b.Size = b.Size + msgSize(msg)
}

// BATCHER PROCESS

const DEFAULT_DELAY = 4 //seconds

// CloudwatchBatcher receieves Cloudwatch messages on its input channel,
// stores them in Slices until enough data is ready to send, then sends each
// CloudwatchMessageBatch on its output channel.
type CloudwatchBatcher struct {
	Input  chan CloudwatchMessage
	output chan CloudwatchBatch
	route  *router.Route
	timer  chan bool
	// maintain a batch for each container, indexed by its name
	batches map[string]*CloudwatchBatch
}

// constructor for CloudwatchBatcher - requires the Logspout Route
func NewCloudwatchBatcher(route *router.Route) *CloudwatchBatcher {
	batcher := CloudwatchBatcher{
		Input:   make(chan CloudwatchMessage),
		output:  NewCloudwatchUploader(route).Input,
		batches: map[string]*CloudwatchBatch{},
		timer:   make(chan bool),
		route:   route,
	}
	go batcher.Start()
	return &batcher
}

// Main loop for the Batcher - just sorts each messages into a batch, but
// submits the batch first and replaces it if the message is too big.
func (b *CloudwatchBatcher) Start() {
	go b.RunTimer()
	for { // run forever, and...
		select { // either batch up a message, or respond to the timer
		case msg := <-b.Input: // a message - put it into its slice
			// get or create the correct slice of messages for this message
			if _, exists := b.batches[msg.Container]; !exists {
				b.batches[msg.Container] = NewCloudwatchBatch()
			}
			// if Msg is too long for the current batch, submit the batch
			if (b.batches[msg.Container].Size + msgSize(msg)) > MAX_BATCH_SIZE {
				b.output <- *b.batches[msg.Container]
				b.batches[msg.Container] = NewCloudwatchBatch()
			}
			thisBatch := b.batches[msg.Container]
			thisBatch.Append(msg)
		case <-b.timer: // submit and delete all existing batches
			for container, batch := range b.batches {
				b.output <- *batch
				delete(b.batches, container)
			}
		}
	}
}

func (b *CloudwatchBatcher) RunTimer() {
	delayText := strconv.Itoa(DEFAULT_DELAY)
	if routeDelay, isSet := b.route.Options[`DELAY`]; isSet {
		delayText = routeDelay
	}
	if envDelay := os.Getenv(`DELAY`); envDelay != "" {
		delayText = envDelay
	}
	delay, err := strconv.Atoi(delayText)
	if err != nil {
		log.Printf("WARNING: ERROR parsing DELAY %s, using default of %d\n",
			delayText, DEFAULT_DELAY)
		delay = DEFAULT_DELAY
	}
	for {
		time.Sleep(time.Duration(delay) * time.Second)
		b.timer <- true
	}
}

// UPLOADER PROCESS

// CloudwatchUploader receieves CloudwatchBatches on its input channel,
// and sends them on to the AWS Cloudwatch Logs endpoint.
type CloudwatchUploader struct {
	Input    chan CloudwatchBatch
	svc      *cloudwatchlogs.CloudWatchLogs
	tokens   map[string]string
	debugSet bool
}

func NewCloudwatchUploader(route *router.Route) *CloudwatchUploader {
	debugSet := false
	_, debugOption := route.Options[`DEBUG`]
	if debugOption || (os.Getenv(`DEBUG`) != "") {
		debugSet = true
		log.Println("cloudwatch: Creating AWS Cloudwatch client for zone",
			route.Address)
	}
	uploader := CloudwatchUploader{
		Input:    make(chan CloudwatchBatch),
		tokens:   map[string]string{},
		debugSet: debugSet,
		svc: cloudwatchlogs.New(session.New(),
			&aws.Config{Region: aws.String(route.Address)}),
	}
	go uploader.Start()
	return &uploader
}

// Main loop for the Uploader - POSTs each batch to AWS Cloudwatch Logs,
// while keeping track of the unique sequence token for each log stream.
func (u *CloudwatchUploader) Start() {
	for batch := range u.Input {
		msg := batch.Msgs[0]
		u.log("Submitting batch for %s-%s (length %d, size %v)",
			msg.Group, msg.Stream, len(batch.Msgs), batch.Size)

		// fetch and cache the upload sequence token
		var token *string
		if cachedToken, isCached := u.tokens[msg.Container]; isCached {
			token = &cachedToken
			u.log("Got token from cache: %s", *token)
		} else {
			u.log("Fetching token from AWS...")
			awsToken, err := u.getSequenceToken(msg)
			if err != nil {
				u.log("ERROR:", err)
				continue
			}
			if awsToken != nil {
				u.tokens[msg.Container] = *(awsToken)
				u.log("Got token from AWS:", *awsToken)
				token = awsToken
			}
		}

		// generate the array of InputLogEvent from the batch's contents
		events := []*cloudwatchlogs.InputLogEvent{}
		for _, msg := range batch.Msgs {
			event := cloudwatchlogs.InputLogEvent{
				Message:   aws.String(msg.Message),
				Timestamp: aws.Int64(msg.Time.UnixNano() / 1000000),
			}
			events = append(events, &event)
		}
		params := &cloudwatchlogs.PutLogEventsInput{
			LogEvents:     events,
			LogGroupName:  aws.String(msg.Group),
			LogStreamName: aws.String(msg.Stream),
			SequenceToken: token,
		}

		u.log("POSTing PutLogEvents to %s-%s with %d messages, %d bytes",
			msg.Group, msg.Stream, len(batch.Msgs), batch.Size)
		resp, err := u.svc.PutLogEvents(params)
		if err != nil {
			u.log(err.Error())
			continue
		}
		u.log("Got 200 response")
		if resp.NextSequenceToken != nil {
			u.log("Caching new sequence token for %s-%s: %s",
				msg.Group, msg.Stream, *resp.NextSequenceToken)
			u.tokens[msg.Container] = *resp.NextSequenceToken
		}
	}
}

// AWS CLIENT METHODS

// returns the next sequence token for the log stream associated
// with the given message's group and stream. Creates the stream as needed.
func (u *CloudwatchUploader) getSequenceToken(msg CloudwatchMessage) (*string,
	error) {
	group, stream := msg.Group, msg.Stream
	groupExists, err := u.groupExists(group)
	if err != nil {
		return nil, err
	}
	if !groupExists {
		err = u.createGroup(group)
		if err != nil {
			return nil, err
		}
	}
	params := &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        aws.String(group),
		LogStreamNamePrefix: aws.String(stream),
	}
	u.log("Describing stream %s-%s...", group, stream)
	resp, err := u.svc.DescribeLogStreams(params)
	if err != nil {
		return nil, err
	}
	if count := len(resp.LogStreams); count > 1 { // too many matching streams!
		return nil, errors.New(fmt.Sprintf(
			"%d streams match group %s, stream %s!", count, group, stream))
	}
	if len(resp.LogStreams) == 0 { // no matching streams - create one and retry
		if err = u.createStream(group, stream); err != nil {
			return nil, err
		}
		token, err := u.getSequenceToken(msg)
		return token, err
	}
	return resp.LogStreams[0].UploadSequenceToken, nil
}

func (u *CloudwatchUploader) groupExists(group string) (bool, error) {
	u.log("Checking for group: %s...", group)
	params := &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(group),
	}
	resp, err := u.svc.DescribeLogGroups(params)
	if err != nil {
		return false, err
	}
	if count := len(resp.LogGroups); count > 1 { // too many matching streams!
		return false, errors.New(fmt.Sprintf(
			"%d groups match group %s!", count, group))
	}
	if count := len(resp.LogGroups); count < 1 { // no matching streams
		return false, nil
	}
	return true, nil
}

func (u *CloudwatchUploader) createGroup(group string) error {
	u.log("Creating group: %s...", group)
	params := &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(group),
	}
	if _, err := u.svc.CreateLogGroup(params); err != nil {
		return err
	}
	return nil
}

func (u *CloudwatchUploader) createStream(group, stream string) error {
	u.log("Creating stream for group %s, stream %s...", group, stream)
	params := &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(group),
		LogStreamName: aws.String(stream),
	}
	if _, err := u.svc.CreateLogStream(params); err != nil {
		return err
	}
	return nil
}

// HELPER METHODS

func (u *CloudwatchUploader) log(format string, args ...interface{}) {
	if u.debugSet {
		msg := fmt.Sprintf(format, args...)
		msg = fmt.Sprintf("cloudwatch: %s", msg)
		if !strings.HasSuffix(msg, "\n") {
			msg = fmt.Sprintf("%s\n", msg)
		}
		log.Print(msg)
	}
}
