package jolokia

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/inputs"
)

// Default http timeouts
var DefaultResponseHeaderTimeout = internal.Duration{Duration: 3 * time.Second}
var DefaultClientTimeout = internal.Duration{Duration: 4 * time.Second}

type Server struct {
	Name     string
	Host     string
	Username string
	Password string
	Port     string
}

type Metric struct {
	Name      string
	Mbean     string
	Attribute string
	Path      string
}

type JolokiaClient interface {
	MakeRequest(req *http.Request) (*http.Response, error)
}

type JolokiaClientImpl struct {
	client *http.Client
}

func (c JolokiaClientImpl) MakeRequest(req *http.Request) (*http.Response, error) {
	return c.client.Do(req)
}

type Jolokia struct {
	jClient JolokiaClient
	Context string
	Mode    string
	Servers []Server
	Metrics []Metric
	Proxy   Server

	ResponseHeaderTimeout internal.Duration `toml:"response_header_timeout"`
	ClientTimeout         internal.Duration `toml:"client_timeout"`
}

const sampleConfig = `
  ## This is the context root used to compose the jolokia url
  ## NOTE that Jolokia requires a trailing slash at the end of the context root
  ## NOTE that your jolokia security policy must allow for POST requests.
  context = "/jolokia/"

  ## This specifies the mode used
  # mode = "proxy"
  #
  ## When in proxy mode this section is used to specify further
  ## proxy address configurations.
  ## Remember to change host address to fit your environment.
  # [inputs.jolokia.proxy]
  #   host = "127.0.0.1"
  #   port = "8080"

  ## Optional http timeouts
  ##
  ## response_header_timeout, if non-zero, specifies the amount of time to wait
  ## for a server's response headers after fully writing the request.
  # response_header_timeout = "3s"
  ##
  ## client_timeout specifies a time limit for requests made by this client.
  ## Includes connection time, any redirects, and reading the response body.
  # client_timeout = "4s"

  ## List of servers exposing jolokia read service
  [[inputs.jolokia.servers]]
    name = "as-server-01"
    host = "127.0.0.1"
    port = "8080"
    # username = "myuser"
    # password = "mypassword"

  ## List of metrics collected on above servers
  ## Each metric consists in a name, a jmx path and either
  ## a pass or drop slice attribute.
  ## This collect all heap memory usage metrics.
  [[inputs.jolokia.metrics]]
    name = "heap_memory_usage"
    mbean  = "java.lang:type=Memory"
    attribute = "HeapMemoryUsage"

  ## This collect thread counts metrics.
  [[inputs.jolokia.metrics]]
    name = "thread_count"
    mbean  = "java.lang:type=Threading"
    attribute = "TotalStartedThreadCount,ThreadCount,DaemonThreadCount,PeakThreadCount"

  ## This collect number of class loaded/unloaded counts metrics.
  [[inputs.jolokia.metrics]]
    name = "class_count"
    mbean  = "java.lang:type=ClassLoading"
    attribute = "LoadedClassCount,UnloadedClassCount,TotalLoadedClassCount"
`

func (j *Jolokia) SampleConfig() string {
	return sampleConfig
}

func (j *Jolokia) Description() string {
	return "Read JMX metrics through Jolokia"
}

func (j *Jolokia) doRequest(req *http.Request) (map[string]interface{}, error) {
	resp, err := j.jClient.MakeRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Process response
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Response from url \"%s\" has status code %d (%s), expected %d (%s)",
			req.RequestURI,
			resp.StatusCode,
			http.StatusText(resp.StatusCode),
			http.StatusOK,
			http.StatusText(http.StatusOK))
		return nil, err
	}

	// read body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Unmarshal json
	var jsonOut map[string]interface{}
	if err = json.Unmarshal([]byte(body), &jsonOut); err != nil {
		return nil, errors.New("Error decoding JSON response")
	}

	if status, ok := jsonOut["status"]; ok {
		if status != float64(200) {
			return nil, fmt.Errorf("Not expected status value in response body: %3.f",
				status)
		}
	} else {
		return nil, fmt.Errorf("Missing status in response body")
	}

	return jsonOut, nil
}

func (j *Jolokia) prepareRequest(server Server, metric Metric) (*http.Request, error) {
	var jolokiaUrl *url.URL
	context := j.Context // Usually "/jolokia/"

	// Create bodyContent
	bodyContent := map[string]interface{}{
		"type":  "read",
		"mbean": metric.Mbean,
	}

	if metric.Attribute != "" {
		bodyContent["attribute"] = metric.Attribute
		if metric.Path != "" {
			bodyContent["path"] = metric.Path
		}
	}

	// Add target, only in proxy mode
	if j.Mode == "proxy" {
		serviceUrl := fmt.Sprintf("service:jmx:rmi:///jndi/rmi://%s:%s/jmxrmi",
			server.Host, server.Port)

		target := map[string]string{
			"url": serviceUrl,
		}

		if server.Username != "" {
			target["user"] = server.Username
		}

		if server.Password != "" {
			target["password"] = server.Password
		}

		bodyContent["target"] = target

		proxy := j.Proxy

		// Prepare ProxyURL
		proxyUrl, err := url.Parse("http://" + proxy.Host + ":" + proxy.Port + context)
		if err != nil {
			return nil, err
		}
		if proxy.Username != "" || proxy.Password != "" {
			proxyUrl.User = url.UserPassword(proxy.Username, proxy.Password)
		}

		jolokiaUrl = proxyUrl

	} else {
		serverUrl, err := url.Parse("http://" + server.Host + ":" + server.Port + context)
		if err != nil {
			return nil, err
		}
		if server.Username != "" || server.Password != "" {
			serverUrl.User = url.UserPassword(server.Username, server.Password)
		}

		jolokiaUrl = serverUrl
	}

	requestBody, err := json.Marshal(bodyContent)

	req, err := http.NewRequest("POST", jolokiaUrl.String(), bytes.NewBuffer(requestBody))

	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-type", "application/json")

	return req, nil
}

func extractValues(measurement string, value interface{}, fields map[string]interface{}) {
	if mapValues, ok := value.(map[string]interface{}); ok {
		for k2, v2 := range mapValues {
			extractValues(measurement+"_"+k2, v2, fields)
		}
	} else {
		fields[measurement] = value
	}
}

func (j *Jolokia) Gather(acc telegraf.Accumulator) error {

	if j.jClient == nil {
		tr := &http.Transport{ResponseHeaderTimeout: j.ResponseHeaderTimeout.Duration}
		j.jClient = &JolokiaClientImpl{&http.Client{
			Transport: tr,
			Timeout:   j.ClientTimeout.Duration,
		}}
	}

	servers := j.Servers
	metrics := j.Metrics
	tags := make(map[string]string)

	for _, server := range servers {
		tags["jolokia_name"] = server.Name
		tags["jolokia_port"] = server.Port
		tags["jolokia_host"] = server.Host
		fields := make(map[string]interface{})

		for _, metric := range metrics {
			measurement := metric.Name

			req, err := j.prepareRequest(server, metric)
			if err != nil {
				return err
			}

			out, err := j.doRequest(req)

			if err != nil {
				fmt.Printf("Error handling response: %s\n", err)
			} else {
				if values, ok := out["value"]; ok {
					extractValues(measurement, values, fields)
				} else {
					fmt.Printf("Missing key 'value' in output response\n")
				}

			}
		}

		acc.AddFields("jolokia", fields, tags)
	}

	return nil
}

func init() {
	inputs.Add("jolokia", func() telegraf.Input {
		return &Jolokia{
			ResponseHeaderTimeout: DefaultResponseHeaderTimeout,
			ClientTimeout:         DefaultClientTimeout,
		}
	})
}
