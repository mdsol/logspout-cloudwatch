package cloudwatch

import (
	"bytes"
	"log"
	"os"
	"strings"
	"text/template"
	"time"

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
	route       *router.Route
	client      *docker.Client
	osHost      string
	batcher     *CloudwatchBatcher // batches up messages by log group and stream
	groupnames  map[string]string  // maps container names to log groups
	streamnames map[string]string  // maps container names to log strams
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
		route:       route,
		client:      client,
		osHost:      hostname,
		batcher:     NewCloudwatchBatcher(route),
		groupnames:  map[string]string{},
		streamnames: map[string]string{},
	}, nil
}

// Stream implements the router.LogAdapter interface.
func (a *CloudwatchAdapter) Stream(logstream chan *router.Message) {
	for m := range logstream {
		// determine the log group name and log stream name
		var groupName, streamName string
		// first, check the in-memory cache so this work is done per-container
		if cachedGroup, isCached := a.groupnames[m.Container.Name]; isCached {
			groupName = cachedGroup
		}
		if cachedStream, isCached := a.streamnames[m.Container.Name]; isCached {
			streamName = cachedStream
		}
		if (streamName == "") || (groupName == "") {
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
			groupName = a.renderEnvValue(`LOG_GROUP`, &context, a.osHost)
			streamName = a.renderEnvValue(`LOG_STREAM`, &context, context.Name)
			a.groupnames[m.Container.Name] = groupName   // cache the group name
			a.streamnames[m.Container.Name] = streamName // and the stream name
		}
		a.batcher.Input <- CloudwatchMessage{
			Message:   m.Data,
			Group:     groupName,
			Stream:    streamName,
			Time:      time.Now(),
			Container: strings.TrimPrefix(m.Container.Name, `/`),
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
