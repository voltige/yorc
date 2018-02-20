package events

import (
	"encoding/json"
	"fmt"
	"path"
	"time"

	"github.com/ystia/yorc/helper/consulutil"
	"github.com/ystia/yorc/log"
)

//go:generate stringer -type=LogLevel -output=log_level_string.go

// LogEntry is the log entry representation
type LogEntry struct {
	level          LogLevel
	deploymentID   string
	additionalInfo LogOptionalFields
	content        []byte
	timestamp      time.Time
}

// LogEntryDraft is a partial LogEntry with only optional fields.
// It has to be completed with level and deploymentID
type LogEntryDraft struct {
	additionalInfo LogOptionalFields
}

// LogOptionalFields are log's additional info
type LogOptionalFields map[FieldType]interface{}

// FieldType is allowed/expected additional info types
type FieldType int

const (
	// WorkFlowID is the field type representing the workflow ID in log entry
	WorkFlowID FieldType = iota

	// ExecutionID is the field type representing the execution ID in log entry
	ExecutionID

	// NodeID is the field type representing the node ID in log entry
	NodeID

	// InstanceID is the field type representing the instance ID in log entry
	InstanceID

	// InterfaceName is the field type representing the interface ID in log entry
	InterfaceName

	// OperationName is the field type representing the operation ID in log entry
	OperationName

	// TypeID is the field type representing the type ID in log entry
	TypeID
)

// String allows to stringify the field type enumeration in JSON standard
func (ft FieldType) String() string {
	switch ft {
	case WorkFlowID:
		return "workflowId"
	case ExecutionID:
		return "executionId"
	case NodeID:
		return "nodeId"
	case InstanceID:
		return "instanceId"
	case InterfaceName:
		return "interfaceName"
	case OperationName:
		return "operationName"
	case TypeID:
		return "type"
	}
	return ""
}

// Max allowed storage size in Consul kv for value is Equal to 512 Kb
// We approximate all data except the content value to be equal to 1Kb
const contentMaxAllowedValueSize int = 511 * 1000

// LogLevel represents the log level enumeration
type LogLevel int

const (
	// INFO is the informative log level
	INFO LogLevel = iota

	// DEBUG is the debugging log level
	DEBUG

	// WARN is the warning log level
	WARN

	// ERROR is the error log level
	ERROR
)

// SimpleLogEntry allows to return a LogEntry instance with log level and deploymentID
func SimpleLogEntry(level LogLevel, deploymentID string) *LogEntry {
	return &LogEntry{
		level:        level,
		deploymentID: deploymentID,
	}
}

// WithOptionalFields allows to return a LogEntry instance with additional fields
func WithOptionalFields(fields LogOptionalFields) *LogEntryDraft {
	info := make(LogOptionalFields, len(fields))
	fle := &LogEntryDraft{additionalInfo: info}
	for k, v := range fields {
		info[k] = v
	}

	return fle
}

// NewLogEntry allows to build a log entry from a draft
func (e LogEntryDraft) NewLogEntry(level LogLevel, deploymentID string) *LogEntry {
	return &LogEntry{
		level:          level,
		deploymentID:   deploymentID,
		additionalInfo: e.additionalInfo,
	}
}

// Register allows to register a log entry with byte array content
func (e LogEntry) Register(content []byte) {
	if len(content) == 0 {
		log.Panic("The content parameter must be filled")
	}
	if e.deploymentID == "" {
		log.Panic("The deploymentID parameter must be filled")
	}
	e.content = content

	// Get the timestamp
	e.timestamp = time.Now()

	// Get the value to store and the flat log entry representation to log entry
	val, flat := e.generateValue()
	err := consulutil.StoreConsulKey(e.generateKey(), val)
	if err != nil {
		log.Printf("Failed to register log in consul for entry:%+v due to error:%+v", e, err)
	}

	// log the entry in stdout/stderr in DEBUG mode
	// Log are only displayed in DEBUG mode
	log.Debugln(FormatLog(flat))
}

// RegisterAsString allows to register a log entry with string content
func (e LogEntry) RegisterAsString(content string) {
	e.Register([]byte(content))
}

// Registerf allows to register a log entry with formats
// according to a format specifier.
//
// This is basically a convenient function around RegisterAsString(fmt.Sprintf()).
func (e LogEntry) Registerf(format string, a ...interface{}) {
	e.RegisterAsString(fmt.Sprintf(format, a...))
}

// RunBufferedRegistration allows to run a registration with a buffered writer
func (e LogEntry) RunBufferedRegistration(buf BufferedLogEntryWriter, quit chan bool) {
	if e.deploymentID == "" {
		log.Panic("The deploymentID parameter must be filled")
	}

	buf.run(quit, e)
}

func (e LogEntry) generateKey() string {
	// time.RFC3339Nano is needed for ConsulKV key value precision
	return path.Join(consulutil.LogsPrefix, e.deploymentID, e.timestamp.Format(time.RFC3339Nano))
}

func (e LogEntry) generateValue() ([]byte, map[string]interface{}) {
	// Check content max allowed size
	if len(e.content) > contentMaxAllowedValueSize {
		log.Printf("The max allowed size has been reached: truncation will be done on log content from %d to %d bytes", len(e.content), contentMaxAllowedValueSize)
		e.content = e.content[:contentMaxAllowedValueSize]
	}
	// For presentation purpose, the log entry is cast to flat map
	flat := e.toFlatMap()
	b, err := json.Marshal(flat)
	if err != nil {
		log.Printf("Failed to marshal entry [%+v]: due to error:%+v", e, err)
	}
	return b, flat
}

func (e LogEntry) toFlatMap() map[string]interface{} {
	flatMap := make(map[string]interface{})

	// NewLogEntry main attributes from LogEntry
	flatMap["deploymentId"] = e.deploymentID
	flatMap["level"] = e.level.String()
	flatMap["content"] = string(e.content)
	flatMap["timestamp"] = e.timestamp.Format(time.RFC3339)

	// NewLogEntry additional info
	for k, v := range e.additionalInfo {
		flatMap[k.String()] = v
	}
	return flatMap
}

// FormatLog allows to format the flat map log representation in the following format :[Timestamp][Level][DeploymentID][WorkflowID][ExecutionID][NodeID][InstanceID][InterfaceName][OperationName][TypeID]Content
func FormatLog(flat map[string]interface{}) string {
	var str string
	sliceOfKeys := []string{"timestamp", "level", "deploymentId", WorkFlowID.String(), ExecutionID.String(), NodeID.String(), InstanceID.String(), InterfaceName.String(), OperationName.String(), TypeID.String(), "content"}
	for _, k := range sliceOfKeys {
		if val, ok := flat[k].(string); ok {
			if k != "content" {
				str += "[" + val + "]"
			} else {
				str += val
			}
		} else {
			str += "[]"
		}

	}
	return str
}
