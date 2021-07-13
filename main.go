package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/google/uuid"
	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type RunnerAddRemove struct {
	Url         string `json:"url"`
	RunnerEvent string `json:"runner_event"`
}

type GitHubAuthResult struct {
	TenantUrl   string `json:"url"`
	TokenSchema string `json:"token_schema"`
	Token       string `json:"token"`
}

type ServiceDefinition struct {
	ServiceType       string
	Identifier        string
	DisplayName       string
	RelativeToSetting int
	RelativePath      string
	Description       string
	ServiceOwner      string
	ResourceVersion   int
}

type LocationServiceData struct {
	ServiceDefinitions []ServiceDefinition
}

type ConnectionData struct {
	LocationServiceData LocationServiceData
}

type TaskAgentPoolReference struct {
	Id         string
	Scope      string
	PoolType   int
	Name       string
	IsHosted   bool
	IsInternal bool
	Size       int
}

type TaskAgentPool struct {
	TaskAgentPoolReference
}

type TaskAgentPublicKey struct {
	Exponent string
	Modulus  string
}

type TaskAgentAuthorization struct {
	AuthorizationUrl string `json:"authorizationUrl,omitempty"`
	ClientId         string `json:"clientId,omitempty"`
	PublicKey        TaskAgentPublicKey
}

type AgentLabel struct {
	Id   int
	Name string
	Type string
}

type TaskAgent struct {
	Authorization  TaskAgentAuthorization
	Labels         []AgentLabel
	MaxParallelism int
	Id             int
	Name           string
	Version        string
	OSDescription  string
	// Enabled           bool
	Status            int
	ProvisioningState string
	// AccessPoint       string
	CreatedOn string
}

type TaskLogReference struct {
	Id       int
	Location *string
}

type TaskLog struct {
	TaskLogReference
	IndexLocation *string `json:"IndexLocation,omitempty"`
	Path          *string `json:"Path,omitempty"`
	LineCount     *int64  `json:"LineCount,omitempty"`
	CreatedOn     string
	LastChangedOn string
}

type TimeLineReference struct {
	Id       string
	ChangeId int
	Location *interface{}
}

type Issue struct {
}

type TimelineAttempt struct {
}

type VariableValue struct {
	Value    string
	IsSecret bool
}

type TimelineRecord struct {
	Id               string
	TimelineId       string
	ParentId         string
	Type             string
	Name             string
	StartTime        string
	FinishTime       *string
	CurrentOperation *string
	PercentComplete  int32
	State            string
	Result           *string
	ResultCode       *string
	ChangeId         int32
	LastModified     string
	WorkerName       string
	Order            int32
	RefName          string
	Log              *TaskLogReference
	Details          *TimeLineReference
	ErrorCount       int
	WarningCount     int
	Issues           []Issue
	Location         string
	Attempt          int32
	Identifier       *string
	AgentPlatform    string
	PreviousAttempts []TimelineAttempt
	Variables        map[string]VariableValue
}

type TaskOrchestrationPlanReference struct {
	ScopeIdentifier string
	PlanId          string
	PlanType        string
}

type MapEntry struct {
	Key   *TemplateToken
	Value *TemplateToken
}

type TemplateToken struct {
	FileId    *int32
	Line      *int32
	Column    *int32
	Type      int32
	Bool      *bool
	Num       *float64
	Lit       *string
	Expr      *string
	Directive *string
	Seq       *[]TemplateToken
	Map       *[]MapEntry
}

func (token *TemplateToken) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &token.Bool) == nil {
		token.Type = 5
		return nil
	} else if json.Unmarshal(data, &token.Num) == nil {
		token.Bool = nil
		token.Type = 6
		return nil
	} else if json.Unmarshal(data, &token.Lit) == nil {
		token.Bool = nil
		token.Num = nil
		token.Type = 0
		return nil
	} else {
		token.Bool = nil
		token.Num = nil
		token.Lit = nil
		type TemplateToken2 TemplateToken
		return json.Unmarshal(data, (*TemplateToken2)(token))
	}
}

func (token *TemplateToken) FromRawObject(value interface{}) {
	switch val := value.(type) {
	case string:
		// TODO: We may need to restore expressions "${{abc}}" to expression objects
		token.Type = 0
		token.Lit = &val
	case []interface{}:
		token.Type = 1
		a := val
		seq := make([]TemplateToken, len(a))
		token.Seq = &seq
		for i, v := range a {
			e := TemplateToken{}
			e.FromRawObject(v)
			(*token.Seq)[i] = e
		}
	case map[interface{}]interface{}:
		token.Type = 2
		_map := make([]MapEntry, 0)
		token.Map = &_map
		for k, v := range val {
			key := &TemplateToken{}
			key.FromRawObject(k)
			value := &TemplateToken{}
			value.FromRawObject(v)
			_map = append(_map, MapEntry{
				Key:   key,
				Value: value,
			})
		}
	case bool:
		token.Type = 5
		token.Bool = &val
	case float64:
		token.Type = 6
		token.Num = &val
	}
}

func (token *TemplateToken) ToRawObject() interface{} {
	switch token.Type {
	case 0:
		return *token.Lit
	case 1:
		a := make([]interface{}, 0)
		for _, v := range *token.Seq {

			a = append(a, v.ToRawObject())
		}
		return a
	case 2:
		m := make(map[interface{}]interface{})
		for _, v := range *token.Map {
			m[v.Key.ToRawObject()] = v.Value.ToRawObject()
		}
		return m
	case 3:
		return "${{" + *token.Expr + "}}"
	case 4:
		return *token.Directive
	case 5:
		return *token.Bool
	case 6:
		return *token.Num
	}
	return nil
}

func (token *TemplateToken) ToYamlNode() *yaml.Node {
	switch token.Type {
	case 0:
		return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: *token.Lit}
	case 1:
		a := make([]*yaml.Node, 0)
		for _, v := range *token.Seq {

			a = append(a, v.ToYamlNode())
		}
		return &yaml.Node{Kind: yaml.SequenceNode, Content: a}
	case 2:
		a := make([]*yaml.Node, 0)
		for _, v := range *token.Map {
			a = append(a, v.Key.ToYamlNode(), v.Value.ToYamlNode())
		}
		return &yaml.Node{Kind: yaml.MappingNode, Content: a}
	case 3:
		return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: "${{" + *token.Expr + "}}"}
	case 4:
		return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: *token.Directive}
	case 5:
		val, _ := yaml.Marshal(token.Bool)
		return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.FlowStyle, Value: string(val[:len(val)-1])}
	case 6:
		val, _ := yaml.Marshal(token.Num)
		return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.FlowStyle, Value: string(val[:len(val)-1])}
	case 7:
		return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.FlowStyle, Value: "null"}
	}
	return nil
}

type JobAuthorization struct {
	Parameters map[string]string
	Scheme     string
}

type JobEndpoint struct {
	Data          map[string]string
	Name          string
	Url           string
	Authorization JobAuthorization
	IsShared      bool
	IsReady       bool
}

type JobResources struct {
	Endpoints []JobEndpoint
}

type DictionaryContextDataPair struct {
	Key   string              `json:"k"`
	Value PipelineContextData `json:"v"`
}

type PipelineContextData struct {
	Type            *int32                       `json:"t,omitempty"`
	BoolValue       *bool                        `json:"b,omitempty"`
	NumberValue     *float64                     `json:"n,omitempty"`
	StringValue     *string                      `json:"s,omitempty"`
	ArrayValue      *[]PipelineContextData       `json:"a,omitempty"`
	DictionaryValue *[]DictionaryContextDataPair `json:"d,omitempty"`
}

func (ctx *PipelineContextData) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &ctx.BoolValue) == nil {
		if ctx.BoolValue == nil {
			ctx = nil
		} else {
			var typ int32 = 3
			ctx.Type = &typ
		}
		return nil
	} else if json.Unmarshal(data, &ctx.NumberValue) == nil {
		ctx.BoolValue = nil
		var typ int32 = 4
		ctx.Type = &typ
		return nil
	} else if json.Unmarshal(data, &ctx.StringValue) == nil {
		ctx.BoolValue = nil
		ctx.NumberValue = nil
		var typ int32 = 0
		ctx.Type = &typ
		return nil
	} else {
		ctx.BoolValue = nil
		ctx.NumberValue = nil
		ctx.StringValue = nil
		type PipelineContextData2 PipelineContextData
		return json.Unmarshal(data, (*PipelineContextData2)(ctx))
	}
}

func (ctx PipelineContextData) ToRawObject() interface{} {
	if ctx.Type == nil {
		return nil
	}
	switch *ctx.Type {
	case 0:
		return *ctx.StringValue
	case 1:
		a := make([]interface{}, 0)
		if ctx.ArrayValue != nil {
			for _, v := range *ctx.ArrayValue {
				a = append(a, v.ToRawObject())
			}
		}
		return a
	case 2:
		m := make(map[string]interface{})
		if ctx.DictionaryValue != nil {
			for _, v := range *ctx.DictionaryValue {
				m[v.Key] = v.Value.ToRawObject()
			}
		}
		return m
	case 3:
		return *ctx.BoolValue
	case 4:
		return *ctx.NumberValue
	}
	return nil
}

type WorkspaceOptions struct {
	Clean *string `json:"Clean,omitempty"`
}

type MaskHint struct {
	Type  string
	Value string
}

type ActionsEnvironmentReference struct {
	Name *string `json:"Name,omitempty"`
	Url  *string `json:"Url,omitempty"`
}

type ActionStepDefinitionReference struct {
	Type           string
	Image          string
	Name           string
	Ref            string
	RepositoryType string
	Path           string
}

type ActionStep struct {
	Type             string
	Reference        ActionStepDefinitionReference
	DisplayNameToken *TemplateToken
	ContextName      string
	Environment      *TemplateToken
	Inputs           *TemplateToken
	Condition        string
	ContinueOnError  *TemplateToken
	TimeoutInMinutes *TemplateToken
}

type AgentJobRequestMessage struct {
	MessageType          string
	Plan                 *TaskOrchestrationPlanReference
	Timeline             *TimeLineReference
	JobId                string
	JobDisplayName       string
	JobName              string
	JobContainer         *TemplateToken
	JobServiceContainers *TemplateToken
	JobOutputs           *TemplateToken
	RequestId            int64
	LockedUntil          string
	Resources            *JobResources
	ContextData          map[string]PipelineContextData
	Workspace            *WorkspaceOptions
	MaskHints            []MaskHint `json:"mask"`
	EnvironmentVariables []TemplateToken
	Defaults             []TemplateToken
	ActionsEnvironment   *ActionsEnvironmentReference
	Variables            map[string]VariableValue
	Steps                []ActionStep
	FileTable            []string
}

type RenewAgent struct {
	RequestId int64
}

type TaskAgentMessage struct {
	MessageId   int64
	MessageType string
	IV          string
	Body        string
}

type TaskAgentSessionKey struct {
	Encrypted bool
	Value     string
}

type TaskAgentSession struct {
	SessionId         string `json:"sessionId,omitempty"`
	EncryptionKey     TaskAgentSessionKey
	OwnerName         string
	Agent             TaskAgent
	UseFipsEncryption bool
}

type VssOAuthTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

type TimelineRecordWrapper struct {
	Count int64
	Value []TimelineRecord
}

type TimelineRecordFeedLinesWrapper struct {
	Count     int32
	Value     []string
	StepId    string
	StartLine *int64
}

type JobEvent struct {
	Name               string
	JobId              string
	RequestId          int64
	Result             string
	Outputs            *map[string]VariableValue    `json:"Outputs,omitempty"`
	ActionsEnvironment *ActionsEnvironmentReference `json:"ActionsEnvironment,omitempty"`
}

func (rec *TimelineRecord) Start() {
	time := time.Now().UTC().Format("2006-01-02T15:04:05")
	rec.PercentComplete = 0
	rec.State = "InProgress"
	rec.StartTime = time
	rec.FinishTime = nil
	rec.LastModified = time
}

func (rec *TimelineRecord) Complete(res string) {
	time := time.Now().UTC().Format("2006-01-02T15:04:05")
	rec.PercentComplete = 100
	rec.State = "Completed"
	rec.FinishTime = &time
	rec.LastModified = time
	rec.Result = &res
}

func CreateTimelineEntry(parent string, refname string, name string) TimelineRecord {
	record := TimelineRecord{}
	record.Id = uuid.New().String()
	record.RefName = refname
	record.Name = name
	record.Type = "Task"
	record.WorkerName = "golang-go"
	record.ParentId = parent
	record.State = "Pending"
	record.LastModified = time.Now().UTC().Format("2006-01-02T15:04:05")
	record.Order = 1
	return record
}

func GetConnectionData(c *http.Client, tenantUrl string) *ConnectionData {
	_url, _ := url.Parse(tenantUrl)
	_url.Path = path.Join(_url.Path, "_apis/connectionData")
	q := _url.Query()
	q.Add("connectOptions", "1")
	q.Add("lastChangeId", "-1")
	q.Add("lastChangeId64", "-1")
	_url.RawQuery = q.Encode()
	connectionData, _ := http.NewRequest("GET", _url.String(), nil)
	connectionDataResp, err := c.Do(connectionData)
	connectionData_ := &ConnectionData{}
	if err != nil {
		fmt.Println("fatal:" + err.Error())
		return nil
	}
	dec2 := json.NewDecoder(connectionDataResp.Body)
	dec2.Decode(connectionData_)
	return connectionData_
}

func BuildUrl(tenantUrl string, relativePath string, ppath map[string]string, query map[string]string) string {
	url2, _ := url.Parse(tenantUrl)
	url := relativePath
	for p, v := range ppath {
		url = strings.ReplaceAll(url, "{"+p+"}", v)
	}
	re := regexp.MustCompile(`/*\{[^\}]+\}`)
	url = re.ReplaceAllString(url, "")
	url2.Path = path.Join(url2.Path, url)
	q := url2.Query()
	for p, v := range query {
		q.Add(p, v)
	}
	url2.RawQuery = q.Encode()
	return url2.String()
}

func (connectionData *ConnectionData) GetServiceDefinition(id string) *ServiceDefinition {
	for i := 0; i < len(connectionData.LocationServiceData.ServiceDefinitions); i++ {
		if connectionData.LocationServiceData.ServiceDefinitions[i].Identifier == id {
			return &connectionData.LocationServiceData.ServiceDefinitions[i]
		}
	}
	return nil
}

func (taskAgent *TaskAgent) CreateSession(connectionData_ *ConnectionData, c *http.Client, tenantUrl string, key *rsa.PrivateKey, token string) (*TaskAgentSession, cipher.Block) {
	session := &TaskAgentSession{}
	session.Agent = *taskAgent
	session.UseFipsEncryption = true
	session.OwnerName = "RUNNER"
	serv := connectionData_.GetServiceDefinition("134e239e-2df3-4794-a6f6-24f1f19ec8dc")
	url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
		"area":     serv.ServiceType,
		"resource": serv.DisplayName,
		"poolId":   fmt.Sprint(1),
	}, map[string]string{})
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.Encode(session)

	poolsreq, _ := http.NewRequest("POST", url, buf)
	poolsreq.Header["Authorization"] = []string{"bearer " + token}
	AddContentType(poolsreq.Header, "6.0-preview")
	AddHeaders(poolsreq.Header)
	poolsresp, _ := c.Do(poolsreq)

	dec := json.NewDecoder(poolsresp.Body)
	dec.Decode(session)
	d, _ := base64.StdEncoding.DecodeString(session.EncryptionKey.Value)
	sessionKey, _ := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, d, []byte{})
	if sessionKey == nil {
		return nil, nil
	}
	b, _ := aes.NewCipher(sessionKey)
	return session, b
}

func (session *TaskAgentSession) Delete(connectionData_ *ConnectionData, c *http.Client, tenantUrl string, token string) error {
	serv := connectionData_.GetServiceDefinition("134e239e-2df3-4794-a6f6-24f1f19ec8dc")
	url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
		"area":      serv.ServiceType,
		"resource":  serv.DisplayName,
		"poolId":    fmt.Sprint(1),
		"sessionId": session.SessionId,
	}, map[string]string{})

	poolsreq, _ := http.NewRequest("DELETE", url, nil)
	poolsreq.Header["Authorization"] = []string{"bearer " + token}
	AddContentType(poolsreq.Header, "6.0-preview")
	AddHeaders(poolsreq.Header)
	poolsresp, _ := c.Do(poolsreq)
	if poolsresp.StatusCode != 200 {
		return errors.New("failed to delete session")
	}
	return nil
}

func AddHeaders(header http.Header) {
	header["X-VSS-E2EID"] = []string{"7f1c293d-97ce-4c59-9e4b-0677c85b8144"}
	header["X-TFS-FedAuthRedirect"] = []string{"Suppress"}
	header["X-TFS-Session"] = []string{"0a6ba747-926b-4ba3-a852-00ab5b5b071a"}
}

func AddContentType(header http.Header, apiversion string) {
	header["Content-Type"] = []string{"application/json; charset=utf-8; api-version=" + apiversion}
	header["Accept"] = []string{"application/json; api-version=" + apiversion}
}

func AddBearer(header http.Header, token string) {
	header["Authorization"] = []string{"bearer " + token}
}

func UpdateTimeLine(con *ConnectionData, c *http.Client, tenantUrl string, timelineId string, jobreq *AgentJobRequestMessage, wrap *TimelineRecordWrapper, token string) {
	serv := con.GetServiceDefinition("8893bc5b-35b2-4be7-83cb-99e683551db4")
	url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
		"area":            serv.ServiceType,
		"resource":        serv.DisplayName,
		"scopeIdentifier": jobreq.Plan.ScopeIdentifier,
		"planId":          jobreq.Plan.PlanId,
		"hubName":         jobreq.Plan.PlanType,
		"timelineId":      timelineId,
	}, map[string]string{})
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.Encode(wrap)

	poolsreq, _ := http.NewRequest("PATCH", url, buf)
	poolsreq.Header["Authorization"] = []string{"bearer " + token}
	AddContentType(poolsreq.Header, "6.0-preview")
	AddHeaders(poolsreq.Header)
	poolsresp, err := c.Do(poolsreq)
	if err != nil {
		fmt.Println("Failed to upload timeline: " + err.Error())
	} else if poolsresp == nil || poolsresp.StatusCode < 200 || poolsresp.StatusCode >= 300 {
		fmt.Println("Failed to upload timeline")
	} else {
		defer poolsresp.Body.Close()
		fmt.Println("Timeline Updated")
	}
}

func UploadLogFile(con *ConnectionData, c *http.Client, tenantUrl string, timelineId string, jobreq *AgentJobRequestMessage, token string, logContent string) int {
	serv := con.GetServiceDefinition("46f5667d-263a-4684-91b1-dff7fdcf64e2")
	log := &TaskLog{}
	{
		url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
			"area":            serv.ServiceType,
			"resource":        serv.DisplayName,
			"scopeIdentifier": jobreq.Plan.ScopeIdentifier,
			"planId":          jobreq.Plan.PlanId,
			"hubName":         jobreq.Plan.PlanType,
			"timelineId":      timelineId,
		}, map[string]string{})

		p := "logs/" + uuid.NewString()
		log.Path = &p
		log.CreatedOn = "2021-05-22T00:00:00"
		log.LastChangedOn = "2021-05-22T00:00:00"

		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		enc.Encode(log)

		poolsreq, _ := http.NewRequest("POST", url, buf)
		AddBearer(poolsreq.Header, token)
		AddContentType(poolsreq.Header, "6.0-preview")
		AddHeaders(poolsreq.Header)
		poolsresp, _ := c.Do(poolsreq)

		if poolsresp.StatusCode != 200 {
			bytes, _ := ioutil.ReadAll(poolsresp.Body)
			fmt.Println(string(bytes))
			fmt.Println(buf.String())
		} else {
			dec := json.NewDecoder(poolsresp.Body)
			dec.Decode(log)
			// bytes, _ := ioutil.ReadAll(poolsresp.Body)
			// fmt.Println(string(bytes))
			// fmt.Println(buf.String())
		}
	}
	{
		url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
			"area":            serv.ServiceType,
			"resource":        serv.DisplayName,
			"scopeIdentifier": jobreq.Plan.ScopeIdentifier,
			"planId":          jobreq.Plan.PlanId,
			"hubName":         jobreq.Plan.PlanType,
			"timelineId":      timelineId,
			"logId":           fmt.Sprint(log.Id),
		}, map[string]string{})

		poolsreq, _ := http.NewRequest("POST", url, bytes.NewBufferString(logContent))
		AddBearer(poolsreq.Header, token)
		AddContentType(poolsreq.Header, "6.0-preview")
		AddHeaders(poolsreq.Header)
		poolsresp, _ := c.Do(poolsreq)

		if poolsresp.StatusCode != 200 {
			bytes, _ := ioutil.ReadAll(poolsresp.Body)
			fmt.Println(string(bytes))
		} else {
			bytes, _ := ioutil.ReadAll(poolsresp.Body)
			fmt.Println(string(bytes))
		}
	}
	return log.Id
}

type ghaFormatter struct {
	rqt            *AgentJobRequestMessage
	rc             *runner.RunContext
	wrap           *TimelineRecordWrapper
	current        *TimelineRecord
	updateTimeLine func()
	logline        func(startLine int64, recordId string, line string)
	uploadLogFile  func(log string) int
	startLine      int64
	stepBuffer     *bytes.Buffer
}

func (f *ghaFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	b := &bytes.Buffer{}

	if f.current == nil || f.current.RefName != f.rc.CurrentStep {
		f.startLine = 1
		if f.current != nil {
			if f.rc.StepResults[f.current.RefName].Success {
				f.current.Complete("Succeeded")
			} else {
				f.current.Complete("Failed")
			}
			if f.stepBuffer.Len() > 0 {
				f.current.Log = &TaskLogReference{Id: f.uploadLogFile(f.stepBuffer.String())}
			}
		}
		f.stepBuffer = &bytes.Buffer{}
		for i := range f.wrap.Value {
			if f.wrap.Value[i].RefName == f.rc.CurrentStep {
				b.WriteString(f.wrap.Value[i].Name)
				b.WriteByte(' ')
				f.current = &f.wrap.Value[i]
				f.current.Start()
				break
			}
		}
		f.updateTimeLine()
	}

	// b.WriteString(f.rc.CurrentStep)
	// b.WriteString(": ")
	if f.rqt.MaskHints != nil {
		for _, v := range f.rqt.MaskHints {
			if strings.ToLower(v.Type) == "regex" {
				r, _ := regexp.Compile(v.Value)
				entry.Message = r.ReplaceAllString(entry.Message, "***")
			}
		}
	}
	if f.rqt.Variables != nil {
		for _, v := range f.rqt.Variables {
			if v.IsSecret && len(v.Value) > 0 {
				entry.Message = strings.ReplaceAll(entry.Message, v.Value, "***")
			}
		}
	}

	b.WriteString(entry.Message)

	f.logline(f.startLine, f.current.Id, strings.Trim(b.String(), "\r\n"))
	f.startLine++
	if entry.Data["raw_output"] != true {
		b.WriteByte('\n')
	}
	f.stepBuffer.Write(b.Bytes())
	return b.Bytes(), nil
}

type ConfigureRunner struct {
	Url    string
	Token  string
	Labels []string
	Name   string
}

func (config *ConfigureRunner) Configure() {
	buf := new(bytes.Buffer)
	req := &RunnerAddRemove{}
	req.Url = config.Url
	req.RunnerEvent = "register"
	enc := json.NewEncoder(buf)
	if err := enc.Encode(req); err != nil {
		return
	}
	registerUrl, err := url.Parse(config.Url)
	if err != nil {
		fmt.Printf("Invalid Url: %v\n", config.Url)
		return
	}
	if strings.ToLower(registerUrl.Host) == "github.com" {
		registerUrl.Host = "api." + registerUrl.Host
		registerUrl.Path = "actions/runner-registration"
	} else {
		registerUrl.Path = "api/v3/actions/runner-registration"
	}
	finalregisterUrl := registerUrl.String()
	fmt.Printf("Try to register runner with url: %v\n", finalregisterUrl)
	r, _ := http.NewRequest("POST", finalregisterUrl, buf)
	r.Header["Authorization"] = []string{"RemoteAuth " + config.Token}
	c := &http.Client{}
	resp, err := c.Do(r)
	if err != nil {
		fmt.Printf("Failed to register Runner: %v\n", err)
		return
	}
	if resp.StatusCode != 200 {
		fmt.Printf("Failed to register Runner with status code: %v\n", resp.StatusCode)
		return
	}

	res := &GitHubAuthResult{}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(res); err != nil {
		fmt.Printf("error decoding struct from JSON: %v\n", err)
		return
	}

	{
		b, _ := json.MarshalIndent(res, "", "    ")
		ioutil.WriteFile("auth.json", b, 0777)
	}
	connectionData_ := GetConnectionData(c, res.TenantUrl)

	{
		serv := connectionData_.GetServiceDefinition("a8c47e17-4d56-4a56-92bb-de7ea7dc65be")
		tenantUrl := res.TenantUrl
		url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
			"area":     serv.ServiceType,
			"resource": serv.DisplayName,
		}, map[string]string{})

		poolsreq, _ := http.NewRequest("GET", url, nil)
		AddBearer(poolsreq.Header, res.Token)
		poolsresp, _ := c.Do(poolsreq)

		bytes, _ := ioutil.ReadAll(poolsresp.Body)

		fmt.Println(string(bytes))
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	ioutil.WriteFile("cred.pkcs1", x509.MarshalPKCS1PrivateKey(key), 0777)

	taskAgent := &TaskAgent{}
	taskAgent.Authorization = TaskAgentAuthorization{}
	bs := make([]byte, 4)
	ui := uint32(key.E)
	binary.BigEndian.PutUint32(bs, ui)
	expof := 0
	for ; expof < 3 && bs[expof] == 0; expof++ {
	}
	taskAgent.Authorization.PublicKey = TaskAgentPublicKey{Exponent: base64.StdEncoding.EncodeToString(bs[expof:]), Modulus: base64.StdEncoding.EncodeToString(key.N.Bytes())}
	taskAgent.Version = "3.0.0"
	taskAgent.OSDescription = "golang"
	taskAgent.Labels = make([]AgentLabel, 1+len(config.Labels))
	taskAgent.Labels[0] = AgentLabel{Name: "self-hosted", Type: "system"}
	for i := 1; i <= len(config.Labels); i++ {
		taskAgent.Labels[i] = AgentLabel{Name: config.Labels[i-1], Type: "user"}
	}
	taskAgent.MaxParallelism = 1
	if config.Name != "" {
		taskAgent.Name = config.Name
	} else {
		taskAgent.Name = "golang_" + uuid.NewString()
	}
	taskAgent.ProvisioningState = "Provisioned"
	taskAgent.CreatedOn = "2021-05-22T00:00:00"
	{
		serv := connectionData_.GetServiceDefinition("e298ef32-5878-4cab-993c-043836571f42")
		tenantUrl := res.TenantUrl
		url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
			"area":     serv.ServiceType,
			"resource": serv.DisplayName,
			"poolId":   fmt.Sprint(1),
		}, map[string]string{})
		{
			poolsreq, _ := http.NewRequest("GET", url, nil)
			AddBearer(poolsreq.Header, res.Token)
			AddContentType(poolsreq.Header, "6.0-preview.2")
			poolsresp, _ := c.Do(poolsreq)

			bytes, _ := ioutil.ReadAll(poolsresp.Body)

			fmt.Println(string(bytes))
		}
		{
			buf := new(bytes.Buffer)
			enc := json.NewEncoder(buf)
			enc.Encode(taskAgent)

			poolsreq, _ := http.NewRequest("POST", url, buf)
			AddBearer(poolsreq.Header, res.Token)
			AddContentType(poolsreq.Header, "6.0-preview.2")
			AddHeaders(poolsreq.Header)
			poolsresp, _ := c.Do(poolsreq)

			if poolsresp.StatusCode != 200 {
				bytes, _ := ioutil.ReadAll(poolsresp.Body)
				fmt.Println(string(bytes))
				fmt.Println(buf.String())
			} else {
				dec := json.NewDecoder(poolsresp.Body)
				dec.Decode(taskAgent)
			}
		}
	}
	b, _ := json.MarshalIndent(taskAgent, "", "    ")
	ioutil.WriteFile("agent.json", b, 0777)
}

type RunRunner struct {
	Once bool
}

func (taskAgent *TaskAgent) Authorize(c *http.Client, key interface{}) (*VssOAuthTokenResponse, error) {
	tokenresp := &VssOAuthTokenResponse{}
	now := time.Now().UTC()
	token2 := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.StandardClaims{
		Subject:   taskAgent.Authorization.ClientId,
		Issuer:    taskAgent.Authorization.ClientId,
		Id:        uuid.New().String(),
		Audience:  taskAgent.Authorization.AuthorizationUrl,
		NotBefore: now.Unix(),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Minute * 5).Unix(),
	})
	stkn, _ := token2.SignedString(key)

	data := url.Values{}
	data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	data.Set("client_assertion", stkn)
	data.Set("grant_type", "client_credentials")

	poolsreq, _ := http.NewRequest("POST", taskAgent.Authorization.AuthorizationUrl, bytes.NewBufferString(data.Encode()))
	poolsreq.Header["Content-Type"] = []string{"application/x-www-form-urlencoded; charset=utf-8"}
	poolsreq.Header["Accept"] = []string{"application/json"}
	poolsresp, err := c.Do(poolsreq)
	if err != nil {
		return nil, errors.New("Failed to Authorize: " + err.Error())
	} else if poolsresp.StatusCode != 200 {
		bytes, _ := ioutil.ReadAll(poolsresp.Body)
		return nil, errors.New("Failed to Authorize, service reponded with code " + fmt.Sprint(poolsresp.StatusCode) + ": " + string(bytes))
	} else {
		dec := json.NewDecoder(poolsresp.Body)
		if err := dec.Decode(tokenresp); err != nil {
			return nil, err
		}
		return tokenresp, nil
	}
}

func ToStringMap(src interface{}) interface{} {
	bi, ok := src.(map[interface{}]interface{})
	if ok {
		res := make(map[string]interface{})
		for k, v := range bi {
			res[k.(string)] = ToStringMap(v)
		}
		return res
	}
	return src
}

func (run *RunRunner) Run() {
	// trap Ctrl+C
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-channel
		cancel()
		fmt.Println("CTRL+C received, stopping accepting new jobs")
	}()
	defer func() {
		cancel()
		signal.Stop(channel)
	}()
	poolId := 1
	c := &http.Client{}
	taskAgent := &TaskAgent{}
	var key *rsa.PrivateKey
	var err error
	req := &GitHubAuthResult{}
	{
		cont, _ := ioutil.ReadFile("agent.json")
		json.Unmarshal(cont, taskAgent)
	}
	{
		cont, err := ioutil.ReadFile("cred.pkcs1")
		if err != nil {
			return
		}
		key, err = x509.ParsePKCS1PrivateKey(cont)
		if err != nil {
			return
		}
	}
	if err != nil {
		return
	}
	{
		cont, _ := ioutil.ReadFile("auth.json")
		json.Unmarshal(cont, req)
	}

	tokenresp, err := taskAgent.Authorize(c, key)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	connectionData_ := GetConnectionData(c, req.TenantUrl)

	session, b := taskAgent.CreateSession(connectionData_, c, req.TenantUrl, key, tokenresp.AccessToken)
	if session == nil {
		fmt.Println("Failed to create Session")
		return
	}
	defer session.Delete(connectionData_, c, req.TenantUrl, tokenresp.AccessToken)
	firstJobReceived := false
	for {
		message := &TaskAgentMessage{}
		success := false
		for !success {
			serv := connectionData_.GetServiceDefinition("c3a054f6-7a8a-49c0-944e-3a8e5d7adfd7")
			url := BuildUrl(req.TenantUrl, serv.RelativePath, map[string]string{
				"area":     serv.ServiceType,
				"resource": serv.DisplayName,
				"poolId":   fmt.Sprint(poolId),
			}, map[string]string{
				"sessionId": session.SessionId,
			})
			//TODO lastMessageId=
			poolsreq, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			AddBearer(poolsreq.Header, tokenresp.AccessToken)
			AddContentType(poolsreq.Header, "6.0-preview")
			AddHeaders(poolsreq.Header)
			poolsresp, err := c.Do(poolsreq)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					fmt.Println("Canceled stopping")
					return
				} else {
					fmt.Printf("Failed to get message: %v\n", err.Error())
				}
			} else if poolsresp == nil {
				fmt.Printf("Failed to get message: Failed without errormessage")
			} else if poolsresp.StatusCode != 200 {
				if poolsresp.StatusCode >= 200 && poolsresp.StatusCode < 300 {
					continue
				}
				// The AccessToken expires every hour
				if poolsresp.StatusCode == 401 {
					tokenresp_, err := taskAgent.Authorize(c, key)
					if err != nil {
						fmt.Println(err.Error())
						return
					}
					tokenresp.AccessToken = tokenresp_.AccessToken
					tokenresp.ExpiresIn = tokenresp_.ExpiresIn
					tokenresp.TokenType = tokenresp_.TokenType
					continue
				}
				bytes, _ := ioutil.ReadAll(poolsresp.Body)
				fmt.Println(string(bytes))
				fmt.Printf("Failed to get message: %v\n", poolsresp.StatusCode)
				return
			} else {
				if firstJobReceived && strings.EqualFold(message.MessageType, "PipelineAgentJobRequest") {
					// It seems run once isn't supported by the backend, do the same as the official runner
					// Skip deleting the job message and cancel earlier
					fmt.Println("Received a second job, but running in run once mode abort")
					return
				}
				success = true
				dec := json.NewDecoder(poolsresp.Body)
				message.MessageType = ""
				dec.Decode(message)
				for {
					url := BuildUrl(req.TenantUrl, serv.RelativePath, map[string]string{
						"area":      serv.ServiceType,
						"resource":  serv.DisplayName,
						"poolId":    fmt.Sprint(poolId),
						"messageId": fmt.Sprint(message.MessageId),
					}, map[string]string{
						"sessionId": session.SessionId,
					})
					poolsreq, _ := http.NewRequest("DELETE", url, nil)
					AddBearer(poolsreq.Header, tokenresp.AccessToken)
					AddContentType(poolsreq.Header, "6.0-preview")
					AddHeaders(poolsreq.Header)
					poolsresp, _ := c.Do(poolsreq)
					if poolsresp.StatusCode != 200 {
						if poolsresp.StatusCode >= 200 && poolsresp.StatusCode < 300 {
							break
						}
						// The AccessToken expires every hour
						if poolsresp.StatusCode == 401 {
							tokenresp_, err := taskAgent.Authorize(c, key)
							if err != nil {
								fmt.Println(err.Error())
								return
							}
							tokenresp.AccessToken = tokenresp_.AccessToken
							tokenresp.ExpiresIn = tokenresp_.ExpiresIn
							tokenresp.TokenType = tokenresp_.TokenType
							continue
						}
						fmt.Print("Failed to delete Message")
						return
					} else {
						break
					}
				}
				if success {
					if strings.EqualFold(message.MessageType, "PipelineAgentJobRequest") {
						if run.Once {
							fmt.Println("First job received")
							firstJobReceived = true
						}
						go func() {
							defer func() {
								if run.Once {
									// cancel Message Loop
									fmt.Println("First job finished, cancel Message loop")
									cancel()
								}
							}()
							iv, _ := base64.StdEncoding.DecodeString(message.IV)
							src, _ := base64.StdEncoding.DecodeString(message.Body)
							cbcdec := cipher.NewCBCDecrypter(b, iv)
							cbcdec.CryptBlocks(src, src)
							maxlen := b.BlockSize()
							validlen := len(src)
							if int(src[len(src)-1]) < maxlen {
								ok := true
								for i := 2; i <= int(src[len(src)-1]); i++ {
									if src[len(src)-i] != src[len(src)-1] {
										ok = false
										break
									}
								}
								if ok {
									validlen -= int(src[len(src)-1])
								}
							}
							off := 0
							// skip utf8 bom, c# cryptostream uses it for utf8
							if src[0] == 239 && src[1] == 187 && src[2] == 191 {
								off = 3
							}
							jobreq := &AgentJobRequestMessage{}
							{
								dec := json.NewDecoder(bytes.NewReader(src[off:validlen]))
								dec.Decode(jobreq)
							}
							jobToken := tokenresp.AccessToken
							jobTenant := req.TenantUrl
							jobConnectionData := connectionData_
							finishJob2 := func(result string, outputs *map[string]VariableValue) {
								finish := &JobEvent{
									Name:      "JobCompleted",
									JobId:     jobreq.JobId,
									RequestId: jobreq.RequestId,
									Result:    result,
									Outputs:   outputs,
								}
								serv := jobConnectionData.GetServiceDefinition("557624af-b29e-4c20-8ab0-0399d2204f3f")
								url := BuildUrl(jobTenant, serv.RelativePath, map[string]string{
									"area":            serv.ServiceType,
									"resource":        serv.DisplayName,
									"scopeIdentifier": jobreq.Plan.ScopeIdentifier,
									"planId":          jobreq.Plan.PlanId,
									"hubName":         jobreq.Plan.PlanType,
								}, map[string]string{})
								buf := new(bytes.Buffer)
								enc := json.NewEncoder(buf)
								enc.Encode(finish)
								poolsreq, _ := http.NewRequest("POST", url, buf)
								AddBearer(poolsreq.Header, jobToken)
								AddContentType(poolsreq.Header, "6.0-preview")
								AddHeaders(poolsreq.Header)
								poolsresp, err := c.Do(poolsreq)
								if err != nil {
									fmt.Printf("Failed to send finish job event: %v\n", err.Error())
								} else if poolsresp == nil {
									fmt.Printf("Failed to send finish job event: Failed without errormessage")
								} else if poolsresp.StatusCode != 200 {
									fmt.Println("Failed to send finish job event with status: " + fmt.Sprint(poolsresp.StatusCode))
								}
							}
							finishJob := func(result string) {
								finishJob2(result, nil)
							}
							rqt := jobreq
							wrap := &TimelineRecordWrapper{}
							wrap.Count = 2
							wrap.Value = make([]TimelineRecord, wrap.Count)
							wrap.Value[0] = CreateTimelineEntry("", rqt.JobName, rqt.JobDisplayName)
							wrap.Value[0].Id = rqt.JobId
							wrap.Value[0].Type = "Job"
							wrap.Value[0].Order = 0
							wrap.Value[0].Start()
							wrap.Value[1] = CreateTimelineEntry(rqt.JobId, "__setup", "Setup Job")
							wrap.Value[1].Order = 1
							wrap.Value[1].Start()
							UpdateTimeLine(jobConnectionData, c, jobTenant, jobreq.Timeline.Id, jobreq, wrap, jobToken)
							failInitJob := func(message string) {
								wrap.Value[1].Log = &TaskLogReference{Id: UploadLogFile(jobConnectionData, c, jobTenant, jobreq.Timeline.Id, jobreq, jobToken, message)}
								wrap.Value[1].Complete("Failed")
								wrap.Value[0].Complete("Failed")
								UpdateTimeLine(jobConnectionData, c, jobTenant, jobreq.Timeline.Id, jobreq, wrap, jobToken)
								fmt.Println(message)
								finishJob("Failed")
							}
							defer func() {
								if err := recover(); err != nil {
									failInitJob("The worker panicked with message: " + fmt.Sprint(err) + "\n" + string(debug.Stack()))
								}
							}()
							if jobreq.Resources == nil {
								failInitJob("Missing Job Resources")
								return
							}
							if jobreq.Resources.Endpoints == nil {
								failInitJob("Missing Job Resources Endpoints")
								return
							}
							orchid := ""
							cacheUrl := ""
							for _, endpoint := range jobreq.Resources.Endpoints {
								if strings.EqualFold(endpoint.Name, "SystemVssConnection") && endpoint.Authorization.Parameters != nil && endpoint.Authorization.Parameters["AccessToken"] != "" {
									jobToken = endpoint.Authorization.Parameters["AccessToken"]
									if jobTenant != endpoint.Url {
										jobTenant = endpoint.Url
										jobConnectionData = GetConnectionData(c, jobTenant)
									}
									claims := jwt.MapClaims{}
									jwt.ParseWithClaims(jobToken, claims, func(t *jwt.Token) (interface{}, error) {
										return nil, nil
									})
									if _orchid, suc := claims["orchid"]; suc {
										orchid = _orchid.(string)
									}
									_cacheUrl, ok := endpoint.Data["CacheServerUrl"]
									if ok {
										cacheUrl = _cacheUrl
									}
								}
							}
							renewctx, cancelRenew := context.WithCancel(context.Background())
							defer cancelRenew()
							go func() {
								for {
									serv := connectionData_.GetServiceDefinition("fc825784-c92a-4299-9221-998a02d1b54f")
									url := BuildUrl(req.TenantUrl, serv.RelativePath, map[string]string{
										"area":      serv.ServiceType,
										"resource":  serv.DisplayName,
										"poolId":    fmt.Sprint(poolId),
										"requestId": fmt.Sprint(jobreq.RequestId),
									}, map[string]string{
										"lockToken": "00000000-0000-0000-0000-000000000000",
									})
									buf := new(bytes.Buffer)
									renew := &RenewAgent{RequestId: jobreq.RequestId}
									enc := json.NewEncoder(buf)
									if err := enc.Encode(renew); err != nil {
										return
									}
									poolsreq, _ := http.NewRequestWithContext(renewctx, "PATCH", url, buf)
									AddBearer(poolsreq.Header, tokenresp.AccessToken)
									AddContentType(poolsreq.Header, "5.1-preview")
									AddHeaders(poolsreq.Header)
									if len(orchid) > 0 {
										poolsreq.Header["X-VSS-OrchestrationId"] = []string{orchid}
									}
									renewresp, err := c.Do(poolsreq)
									if err != nil {
										if errors.Is(err, context.Canceled) {
											return
										} else {
											fmt.Printf("Failed to renew job: %v\n", err.Error())
										}
									} else if renewresp != nil {
										defer renewresp.Body.Close()
										if renewresp.StatusCode < 200 || renewresp.StatusCode >= 300 {
											fmt.Printf("Failed to renew job with Http Status: %v\n", renewresp.StatusCode)
										}
									}
									select {
									case <-renewctx.Done():
										return
									case <-time.After(60 * time.Second):
									}
								}
							}()

							rawGithubCtx, ok := rqt.ContextData["github"]
							if !ok {
								fmt.Println("missing github context in ContextData")
								finishJob("Failed")
								return
							}
							githubCtx := rawGithubCtx.ToRawObject()
							secrets := map[string]string{}
							if rqt.Variables != nil {
								for k, v := range rqt.Variables {
									if v.IsSecret && k != "system.github.token" {
										secrets[k] = v.Value
									}
								}
								if rawGithubToken, ok := rqt.Variables["system.github.token"]; ok {
									secrets["GITHUB_TOKEN"] = rawGithubToken.Value
								}
							}
							matrix := make(map[string]interface{})
							if rawMatrix, ok := rqt.ContextData["matrix"]; ok {
								rawobj := rawMatrix.ToRawObject()
								if tmpmatrix, ok := rawobj.(map[string]interface{}); ok {
									matrix = tmpmatrix
								} else if rawobj != nil {
									failInitJob("matrix: not a map")
									return
								}
							}
							env := make(map[string]string)
							if rqt.EnvironmentVariables != nil {
								for _, rawenv := range rqt.EnvironmentVariables {
									if tmpenv, ok := rawenv.ToRawObject().(map[interface{}]interface{}); ok {
										for k, v := range tmpenv {
											key, ok := k.(string)
											if !ok {
												failInitJob("env key: act doesn't support non strings")
												return
											}
											value, ok := v.(string)
											if !ok {
												failInitJob("env value: act doesn't support non strings")
												return
											}
											env[key] = value
										}
									} else {
										failInitJob("env: not a map")
										return
									}
								}
							}
							env["ACTIONS_RUNTIME_URL"] = jobTenant
							env["ACTIONS_RUNTIME_TOKEN"] = jobToken
							if len(cacheUrl) > 0 {
								env["ACTIONS_CACHE_URL"] = cacheUrl
							}

							defaults := model.Defaults{}
							if rqt.Defaults != nil {
								for _, rawenv := range rqt.Defaults {
									rawobj := rawenv.ToRawObject()
									rawobj = ToStringMap(rawobj)
									b, err := json.Marshal(rawobj)
									if err != nil {
										failInitJob("Failed to eval defaults")
										return
									}
									json.Unmarshal(b, &defaults)
								}
							}
							steps := []*model.Step{}
							for _, step := range rqt.Steps {
								st := strings.ToLower(step.Reference.Type)
								inputs := make(map[interface{}]interface{})
								if step.Inputs != nil {
									if tmpinputs, ok := step.Inputs.ToRawObject().(map[interface{}]interface{}); ok {
										inputs = tmpinputs
									} else {
										failInitJob("step.Inputs: not a map")
										return
									}
								}
								env := make(map[string]string)
								if step.Environment != nil {
									if tmpenvs, ok := step.Environment.ToRawObject().(map[interface{}]interface{}); ok {
										for k, v := range tmpenvs {
											key, ok := k.(string)
											if !ok {
												failInitJob("env key: act doesn't support non strings")
												return
											}
											value, ok := v.(string)
											if !ok {
												failInitJob("env value: act doesn't support non strings")
												return
											}
											env[key] = value
										}
									} else {
										failInitJob("step.Inputs: not a map")
										return
									}
								}

								rawwd, haswd := inputs["workingDirectory"]
								var wd string
								if haswd {
									tmpwd, ok := rawwd.(string)
									if !ok {
										failInitJob("workingDirectory: act doesn't support non strings")
										return
									}
									wd = tmpwd
								} else {
									wd = ""
								}
								continueOnError := false
								if step.ContinueOnError != nil {
									tmpcontinueOnError, ok := step.ContinueOnError.ToRawObject().(bool)
									if !ok {
										failInitJob("ContinueOnError: act doesn't support expressions here")
										return
									}
									continueOnError = tmpcontinueOnError
								}
								var timeoutMinutes int64 = 0
								if step.TimeoutInMinutes != nil {
									rawTimeout, ok := step.TimeoutInMinutes.ToRawObject().(float64)
									if !ok {
										failInitJob("TimeoutInMinutes: act doesn't support expressions here")
										return
									}
									timeoutMinutes = int64(rawTimeout)
								}
								var displayName string = ""
								if step.DisplayNameToken != nil {
									rawDisplayName, ok := step.DisplayNameToken.ToRawObject().(string)
									if !ok {
										failInitJob("DisplayNameToken: act doesn't support no strings")
										return
									}
									displayName = rawDisplayName
								}
								if step.ContextName == "" {
									step.ContextName = "___" + uuid.New().String()
								}

								switch st {
								case "script":
									rawshell, hasshell := inputs["shell"]
									shell := ""
									if hasshell {
										sshell, ok := rawshell.(string)
										if ok {
											shell = sshell
										} else {
											failInitJob("shell is not a script")
											return
										}
									}
									scriptContent, ok := inputs["script"].(string)
									if ok {
										steps = append(steps, &model.Step{
											ID:               step.ContextName,
											If:               yaml.Node{Kind: yaml.ScalarNode, Value: step.Condition},
											Name:             displayName,
											Run:              scriptContent,
											WorkingDirectory: wd,
											Shell:            shell,
											ContinueOnError:  continueOnError,
											TimeoutMinutes:   timeoutMinutes,
											Env:              env,
										})
									} else {
										failInitJob("Missing script")
										return
									}
								case "containerregistry", "repository":
									uses := ""
									if st == "containerregistry" {
										uses = "docker://" + step.Reference.Image
									} else if strings.ToLower(step.Reference.RepositoryType) == "self" {
										uses = step.Reference.Path
									} else {
										uses = step.Reference.Name
										if len(step.Reference.Path) > 0 {
											uses = uses + "/" + step.Reference.Path
										}
										uses = uses + "@" + step.Reference.Ref
									}
									with := map[string]string{}
									for k, v := range inputs {
										k, ok := k.(string)
										if !ok {
											failInitJob("with input key is not a string")
											return
										}
										val, ok := v.(string)
										if !ok {
											fmt.Println("with input value is not a string")
											return
										}
										with[k] = val
									}

									steps = append(steps, &model.Step{
										ID:               step.ContextName,
										If:               yaml.Node{Kind: yaml.ScalarNode, Value: step.Condition},
										Name:             displayName,
										Uses:             uses,
										WorkingDirectory: "",
										With:             with,
										ContinueOnError:  continueOnError,
										TimeoutMinutes:   timeoutMinutes,
										Env:              env,
									})
								}
							}
							rawContainer := yaml.Node{}
							if rqt.JobContainer != nil {
								rawContainer = *rqt.JobContainer.ToYamlNode()
							}
							services := make(map[string]*model.ContainerSpec)
							if rqt.JobServiceContainers != nil {
								rawServiceContainer, ok := rqt.JobServiceContainers.ToRawObject().(map[interface{}]interface{})
								if !ok {
									failInitJob("Job service container is not nil, but also not a map")
									return
								}
								for name, rawcontainer := range rawServiceContainer {
									containerName, ok := name.(string)
									if !ok {
										failInitJob("containername is not a string")
										return
									}
									spec := &model.ContainerSpec{}
									b, err := json.Marshal(ToStringMap(rawcontainer))
									if err != nil {
										failInitJob("Failed to serialize ContainerSpec")
										return
									}
									err = json.Unmarshal(b, &spec)
									if err != nil {
										failInitJob("Failed to deserialize ContainerSpec")
										return
									}
									services[containerName] = spec
								}
							}
							githubCtxMap, ok := githubCtx.(map[string]interface{})
							if !ok {
								failInitJob("Github ctx is not a map")
								return
							}
							var payload string
							{
								e, _ := json.Marshal(githubCtxMap["event"])
								payload = string(e)
							}
							rc := &runner.RunContext{
								Config: &runner.Config{
									Workdir: ".",
									Secrets: secrets,
									Platforms: map[string]string{
										"dummy": "-self-hosted",
									},
									LogOutput:           true,
									EventName:           githubCtxMap["event_name"].(string),
									GitHubInstance:      githubCtxMap["server_url"].(string)[8:],
									ForceRemoteCheckout: true, // Needed to avoid copy the non exiting working dir
								},
								Env: env,
								Run: &model.Run{
									JobID: rqt.JobId,
									Workflow: &model.Workflow{
										Name:     githubCtxMap["workflow"].(string),
										Defaults: defaults,
										Jobs: map[string]*model.Job{
											rqt.JobId: {
												Name:         rqt.JobDisplayName,
												RawRunsOn:    yaml.Node{Kind: yaml.ScalarNode, Value: "dummy"},
												Steps:        steps,
												RawContainer: rawContainer,
												Services:     services,
												Outputs:      make(map[string]string),
											},
										},
									},
								},
								Matrix:    matrix,
								EventJSON: payload,
							}

							// Prepare act to fill previous job outputs
							if rawNeedstx, ok := rqt.ContextData["needs"]; ok {
								needsCtx := rawNeedstx.ToRawObject()
								if needsCtxMap, ok := needsCtx.(map[string]interface{}); ok {
									a := make([]*yaml.Node, 0)
									for k, v := range needsCtxMap {
										a = append(a, &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: k})
										outputs := make(map[string]string)
										if jobMap, ok := v.(map[string]interface{}); ok {
											if jobOutputs, ok := jobMap["outputs"]; ok {
												if outputMap, ok := jobOutputs.(map[string]interface{}); ok {
													for k, v := range outputMap {
														if sv, ok := v.(string); ok {
															outputs[k] = sv
														}
													}
												}
											}
										}
										rc.Run.Workflow.Jobs[k] = &model.Job{
											Outputs: outputs,
										}
									}
									rc.Run.Workflow.Jobs[rqt.JobId].RawNeeds = yaml.Node{Kind: yaml.SequenceNode, Content: a}
								}
							}
							// Prepare act to add job outputs to current job
							if rqt.JobOutputs != nil {
								o := rqt.JobOutputs.ToRawObject()
								if m, ok := o.(map[interface{}]interface{}); ok {
									for k, v := range m {
										if kv, ok := k.(string); ok {
											if sv, ok := v.(string); ok {
												rc.Run.Workflow.Jobs[rqt.JobId].Outputs[kv] = sv
											}
										}
									}
								}
							}

							val, _ := json.Marshal(githubCtx)
							sv := string(val)
							rc.GithubContextBase = &sv
							rc.JobName = "beta"

							ctx := context.Background()

							ee := rc.NewExpressionEvaluator()
							rc.ExprEval = ee
							logger := logrus.New()

							buf := new(bytes.Buffer)

							formatter := new(ghaFormatter)
							formatter.rc = rc
							formatter.rqt = rqt
							formatter.stepBuffer = &bytes.Buffer{}

							logger.SetFormatter(formatter)
							logger.SetOutput(buf)
							logger.SetLevel(logrus.DebugLevel)

							rc.CurrentStep = "__setup"
							rc.InitStepResults([]string{rc.CurrentStep})

							for i := 0; i < len(steps); i++ {
								wrap.Value = append(wrap.Value, CreateTimelineEntry(rqt.JobId, steps[i].ID, steps[i].String()))
								wrap.Value[i+2].Order = int32(i + 2)
							}
							formatter.current = &wrap.Value[1]
							wrap.Count = int64(len(wrap.Value))
							UpdateTimeLine(jobConnectionData, c, jobTenant, jobreq.Timeline.Id, jobreq, wrap, jobToken)
							{
								formatter.updateTimeLine = func() {
									UpdateTimeLine(jobConnectionData, c, jobTenant, jobreq.Timeline.Id, jobreq, wrap, jobToken)
								}
								formatter.uploadLogFile = func(log string) int {
									return UploadLogFile(jobConnectionData, c, jobTenant, jobreq.Timeline.Id, jobreq, jobToken, log)
								}
							}
							{
								serv := jobConnectionData.GetServiceDefinition("858983e4-19bd-4c5e-864c-507b59b58b12")
								tenantUrl := jobTenant
								logchan := make(chan *TimelineRecordFeedLinesWrapper, 64)
								formatter.logline = func(startLine int64, recordId string, line string) {
									lines := &TimelineRecordFeedLinesWrapper{}
									lines.Count = 1
									lines.StartLine = &startLine
									lines.StepId = recordId
									lines.Value = []string{line}
									logchan <- lines
								}
								go func() {
									sendLog := func(lines *TimelineRecordFeedLinesWrapper) {
										url := BuildUrl(tenantUrl, serv.RelativePath, map[string]string{
											"area":            serv.ServiceType,
											"resource":        serv.DisplayName,
											"scopeIdentifier": jobreq.Plan.ScopeIdentifier,
											"planId":          jobreq.Plan.PlanId,
											"hubName":         jobreq.Plan.PlanType,
											"timelineId":      jobreq.Timeline.Id,
											"recordId":        lines.StepId,
										}, map[string]string{})

										buf := new(bytes.Buffer)
										enc := json.NewEncoder(buf)

										enc.Encode(lines)
										poolsreq, _ := http.NewRequest("POST", url, buf)
										AddBearer(poolsreq.Header, jobToken)
										AddContentType(poolsreq.Header, "6.0-preview")
										AddHeaders(poolsreq.Header)
										resp, err := c.Do(poolsreq)
										if err != nil {
											fmt.Println("Failed to upload logline: " + err.Error())
										} else if resp == nil || resp.StatusCode != 200 {
											fmt.Println("Failed to upload logline")
										}
									}
									for {
										select {
										case <-renewctx.Done():
											return
										case lines := <-logchan:
											st := time.Now()
											lp := st
											for {
												b := false
												div := lp.Sub(st)
												if div > time.Second {
													break
												}
												select {
												case line := <-logchan:
													if line.StepId == lines.StepId {
														lines.Count++
														lines.Value = append(lines.Value, line.Value[0])
													} else {
														sendLog(lines)
														lines = line
														st = time.Now()
													}
												case <-time.After(time.Second - div):
													b = true
												}
												if b {
													break
												}
												lp = time.Now()
											}
											sendLog(lines)
										}
									}
								}()
							}
							formatter.wrap = wrap

							rc.Executor()(common.WithLogger(ctx, logger))

							// Prepare results for github server
							var outputMap *map[string]VariableValue
							if rqt.JobOutputs != nil {
								m := make(map[string]VariableValue)
								outputMap = &m
								for k, v := range rc.Run.Workflow.Jobs[rqt.JobId].Outputs {
									m[k] = VariableValue{Value: v}
								}
							}

							jobStatus := "success"
							for _, stepStatus := range rc.StepResults {
								if !stepStatus.Success {
									jobStatus = "failure"
									break
								}
							}
							{
								f := formatter
								f.startLine = 1
								if f.current != nil {
									if f.current == &wrap.Value[1] {
										// Workaround check for init failure, e.g. docker fails
										jobStatus = "failure"
										f.current.Complete("Failed")
									} else if f.rc.StepResults[f.current.RefName].Success {
										f.current.Complete("Succeeded")
									} else {
										f.current.Complete("Failed")
									}
									if f.stepBuffer.Len() > 0 {
										f.current.Log = &TaskLogReference{Id: f.uploadLogFile(f.stepBuffer.String())}
									}
								}
							}
							for i := 2; i < len(wrap.Value); i++ {
								if !strings.EqualFold(wrap.Value[i].State, "Completed") {
									wrap.Value[i].Complete("Skipped")
								}
							}
							if jobStatus == "success" {
								wrap.Value[0].Complete("Succeeded")
							} else {
								wrap.Value[0].Complete("Failed")
							}

							UpdateTimeLine(jobConnectionData, c, jobTenant, jobreq.Timeline.Id, jobreq, wrap, jobToken)
							fmt.Println("Finishing Job")
							result := "Failed"
							if jobStatus == "success" {
								result = "Succeeded"
							}
							finishJob2(result, outputMap)
							fmt.Println("Finished Job")
						}()
					} else {
						fmt.Println("Ignoring incoming message of type: " + message.MessageType)
					}
				}
			}
		}
	}
}

func main() {
	config := &ConfigureRunner{}
	run := &RunRunner{}
	var cmdConfigure = &cobra.Command{
		Use:   "Configure",
		Short: "Configure your self-hosted runner",
		Args:  cobra.MaximumNArgs(0),
		Run: func(cmd *cobra.Command, args []string) {
			config.Configure()
		},
	}

	cmdConfigure.Flags().StringVar(&config.Url, "url", "", "url of your repository or enterprise")
	cmdConfigure.Flags().StringVar(&config.Token, "token", "", "runner registration token")
	cmdConfigure.Flags().StringSliceVarP(&config.Labels, "label", "l", []string{}, "label for your new runner")
	cmdConfigure.Flags().StringVar(&config.Name, "name", "", "custom runner name")

	var cmdRun = &cobra.Command{
		Use:   "Run",
		Short: "run your self-hosted runner",
		Args:  cobra.MaximumNArgs(0),
		Run: func(cmd *cobra.Command, args []string) {
			run.Run()
		},
	}

	cmdRun.Flags().BoolVar(&run.Once, "once", false, "only execute one job and exit")

	var rootCmd = &cobra.Command{Use: "github-actions-act-runner"}
	rootCmd.AddCommand(cmdConfigure, cmdRun)
	rootCmd.Execute()
}
