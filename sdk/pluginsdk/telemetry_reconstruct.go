package pluginsdk

import (
	"strconv"
	"sync"
	"time"

	gcpcv1 "gocache/api/gcpc/v1"
)

const (
	telemetryFieldArgsCount = "_args_count"
	telemetryFieldArgPrefix = "_arg."
	telemetryFieldCaller    = "_caller"
	telemetryFieldCommand   = "_command"
	telemetryFieldElapsedNs = "_elapsed_ns"
	telemetryFieldError     = "_error"
	telemetryFieldLevel     = "_level"
	telemetryFieldMessage   = "_message"
	telemetryFieldResult    = "_result"
	telemetryFieldStatus    = "_status"
)

// ReconstructedOperation is the output of context reconstruction.
type ReconstructedOperation struct {
	OperationID string
	Context     map[string]string
	Commands    []ReconstructedCommand
	Logs        []ReconstructedLog
	Events      []ReconstructedEvent
	Elapsed     time.Duration
	Status      string
}

// ReconstructedCommand is a reconstructed command boundary timeline entry.
type ReconstructedCommand struct {
	Name    string
	Args    []string
	Elapsed time.Duration
	Result  string
	Error   string
}

// ReconstructedLog is a reconstructed log request entry.
type ReconstructedLog struct {
	Level   string
	Message string
	Caller  string
}

// ReconstructedEvent is a reconstructed event request entry.
type ReconstructedEvent struct {
	Type string
	Data map[string]string
}

// ContextReconstructor maintains per-operation context state machines.
// It is safe for concurrent use; each operation is tracked by ID.
type ContextReconstructor struct {
	states map[string]*reconstructionState
	mu     sync.Mutex
}

type reconstructionState struct {
	context  map[string]string
	commands []ReconstructedCommand
	logs     []ReconstructedLog
	events   []ReconstructedEvent
}

type telemetryPair struct {
	key  string
	text string
}

// NewContextReconstructor creates an empty telemetry context reconstructor.
func NewContextReconstructor() *ContextReconstructor {
	return &ContextReconstructor{states: make(map[string]*reconstructionState)}
}

// ProcessOperation deserializes a TelemetryOperation and reconstructs the full operation state.
// For OperationFinish items, returns a completed ReconstructedOperation.
// For non-finish operations, returns nil because the operation is still accumulating.
func (r *ContextReconstructor) ProcessOperation(op *gcpcv1.TelemetryOperation) *ReconstructedOperation {
	if r == nil || op == nil {
		return nil
	}
	operationID := op.GetOperationId()
	if operationID == "" {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.stateForOperation(operationID, op.GetInitialContext())
	for _, telemetryItem := range op.GetTelemetryItems() {
		if telemetryItem == nil {
			continue
		}
		if reconstructed := r.applyTelemetryItem(operationID, state, telemetryItem); reconstructed != nil {
			delete(r.states, operationID)
			return reconstructed
		}
	}

	return nil
}

func (r *ContextReconstructor) stateForOperation(operationID string, initialContext []*gcpcv1.Tag) *reconstructionState {
	if state, ok := r.states[operationID]; ok {
		return state
	}
	state := &reconstructionState{context: contextFromTelemetryTags(initialContext)}
	r.states[operationID] = state
	return state
}

func (r *ContextReconstructor) applyTelemetryItem(operationID string, state *reconstructionState, telemetryItem *gcpcv1.TelemetryItem) *ReconstructedOperation {
	switch telemetryItem.GetKind() {
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_START:
		return nil
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH:
		return finishReconstructedOperation(operationID, state, telemetryItem.GetPayload())
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_START:
		state.commands = append(state.commands, commandStartFromPayload(telemetryItem.GetPayload()))
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_FINISH:
		state.commands = applyCommandFinishPayload(state.commands, telemetryItem.GetPayload())
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE:
		applyContextUpdatePayload(state.context, telemetryItem.GetPayload())
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_REMOVE:
		applyContextRemovePayload(state.context, telemetryItem.GetPayload())
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_LOG:
		state.logs = append(state.logs, logFromPayload(telemetryItem.GetPayload()))
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_EVENT:
		state.events = append(state.events, eventFromPayload(telemetryItem.GetPayload()))
	case gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_DROP,
		gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_UNSPECIFIED:
		return nil
	}
	return nil
}

func contextFromTelemetryTags(tags []*gcpcv1.Tag) map[string]string {
	operationContext := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag == nil {
			continue
		}
		operationContext[string(tag.GetKey())] = string(tag.GetValue())
	}
	return operationContext
}

func finishReconstructedOperation(operationID string, state *reconstructionState, payload []byte) *ReconstructedOperation {
	fields := telemetryPairsFromPayload(payload)
	status := telemetryPairValue(fields, telemetryFieldStatus)
	if status == "" {
		status = "completed"
	}
	return &ReconstructedOperation{
		OperationID: operationID,
		Context:     cloneTelemetryContext(state.context),
		Commands:    cloneReconstructedCommands(state.commands),
		Logs:        cloneReconstructedLogs(state.logs),
		Events:      cloneReconstructedEvents(state.events),
		Elapsed:     durationFromNanosecondsField(fields, telemetryFieldElapsedNs),
		Status:      status,
	}
}

func applyContextUpdatePayload(operationContext map[string]string, payload []byte) {
	fields := telemetryPairsFromPayload(payload)
	for _, field := range fields {
		operationContext[field.key] = field.text
	}
}

func applyContextRemovePayload(operationContext map[string]string, payload []byte) {
	if len(operationContext) == 0 || len(payload) == 0 {
		return
	}
	position := 1
	for remaining := int(payload[0]); remaining > 0; remaining-- {
		if position >= len(payload) {
			return
		}
		keyLength := int(payload[position])
		position++
		if position+keyLength > len(payload) {
			return
		}
		delete(operationContext, string(payload[position:position+keyLength]))
		position += keyLength
	}
}

func commandStartFromPayload(payload []byte) ReconstructedCommand {
	fields := telemetryPairsFromPayload(payload)
	return ReconstructedCommand{
		Name: telemetryPairValue(fields, telemetryFieldCommand),
		Args: commandArgsFromPairs(fields),
	}
}

func applyCommandFinishPayload(commands []ReconstructedCommand, payload []byte) []ReconstructedCommand {
	fields := telemetryPairsFromPayload(payload)
	finishedCommand := ReconstructedCommand{
		Name:    telemetryPairValue(fields, telemetryFieldCommand),
		Args:    commandArgsFromPairs(fields),
		Elapsed: durationFromNanosecondsField(fields, telemetryFieldElapsedNs),
		Result:  telemetryPairValue(fields, telemetryFieldResult),
		Error:   telemetryPairValue(fields, telemetryFieldError),
	}
	for commandIndex := len(commands) - 1; commandIndex >= 0; commandIndex-- {
		if commands[commandIndex].Name != finishedCommand.Name {
			continue
		}
		commands[commandIndex].Elapsed = finishedCommand.Elapsed
		commands[commandIndex].Result = finishedCommand.Result
		commands[commandIndex].Error = finishedCommand.Error
		if len(commands[commandIndex].Args) == 0 {
			commands[commandIndex].Args = finishedCommand.Args
		}
		return commands
	}
	return append(commands, finishedCommand)
}

func logFromPayload(payload []byte) ReconstructedLog {
	fields := telemetryPairsFromPayload(payload)
	return ReconstructedLog{
		Level:   telemetryPairValue(fields, telemetryFieldLevel),
		Message: telemetryPairValue(fields, telemetryFieldMessage),
		Caller:  telemetryPairValue(fields, telemetryFieldCaller),
	}
}

func eventFromPayload(payload []byte) ReconstructedEvent {
	fields := telemetryPairsFromPayload(payload)
	eventFields := make(map[string]string, len(fields))
	eventType := ""
	for _, field := range fields {
		if field.key == "_type" {
			eventType = field.text
			continue
		}
		eventFields[field.key] = field.text
	}
	return ReconstructedEvent{Type: eventType, Data: eventFields}
}

func telemetryPairsFromPayload(payload []byte) []telemetryPair {
	if len(payload) == 0 {
		return nil
	}
	position := 1
	fields := make([]telemetryPair, 0, int(payload[0]))
	for remaining := int(payload[0]); remaining > 0; remaining-- {
		if position >= len(payload) {
			return fields
		}
		keyLength := int(payload[position])
		position++
		if position+keyLength > len(payload) {
			return fields
		}
		key := string(payload[position : position+keyLength])
		position += keyLength
		if position >= len(payload) {
			return fields
		}
		valueLength := int(payload[position])
		position++
		if position+valueLength > len(payload) {
			return fields
		}
		fieldText := string(payload[position : position+valueLength])
		position += valueLength
		fields = append(fields, telemetryPair{key: key, text: fieldText})
	}
	return fields
}

func telemetryPairValue(fields []telemetryPair, key string) string {
	for pairIndex := len(fields) - 1; pairIndex >= 0; pairIndex-- {
		if fields[pairIndex].key == key {
			return fields[pairIndex].text
		}
	}
	return ""
}

func commandArgsFromPairs(fields []telemetryPair) []string {
	argsCountText := telemetryPairValue(fields, telemetryFieldArgsCount)
	argsCount, err := strconv.Atoi(argsCountText)
	if err != nil || argsCount <= 0 {
		return nil
	}
	args := make([]string, argsCount)
	for _, field := range fields {
		if len(field.key) <= len(telemetryFieldArgPrefix) || field.key[:len(telemetryFieldArgPrefix)] != telemetryFieldArgPrefix {
			continue
		}
		argIndex, ok := parseTelemetryArgIndex(field.key[len(telemetryFieldArgPrefix):])
		if ok && argIndex < argsCount {
			args[argIndex] = field.text
		}
	}
	return args
}

func parseTelemetryArgIndex(indexText string) (int, bool) {
	if indexText == "" || (len(indexText) > 1 && indexText[0] == '0') {
		return 0, false
	}
	argIndex := 0
	for charIndex := 0; charIndex < len(indexText); charIndex++ {
		char := indexText[charIndex]
		if char < '0' || char > '9' {
			return 0, false
		}
		argIndex = argIndex*10 + int(char-'0')
		if argIndex < 0 {
			return 0, false
		}
	}
	return argIndex, true
}

func durationFromNanosecondsField(fields []telemetryPair, key string) time.Duration {
	fieldText := telemetryPairValue(fields, key)
	if fieldText == "" {
		return 0
	}
	elapsedNanoseconds, err := strconv.ParseInt(fieldText, 10, 64)
	if err != nil || elapsedNanoseconds <= 0 {
		return 0
	}
	return time.Duration(elapsedNanoseconds)
}

func cloneTelemetryContext(operationContext map[string]string) map[string]string {
	if len(operationContext) == 0 {
		return nil
	}
	clonedContext := make(map[string]string, len(operationContext))
	for key, contextValue := range operationContext {
		clonedContext[key] = contextValue
	}
	return clonedContext
}

func cloneReconstructedCommands(commands []ReconstructedCommand) []ReconstructedCommand {
	if len(commands) == 0 {
		return nil
	}
	clonedCommands := make([]ReconstructedCommand, len(commands))
	for commandIndex, command := range commands {
		clonedCommands[commandIndex] = command
		if len(command.Args) > 0 {
			clonedCommands[commandIndex].Args = append([]string(nil), command.Args...)
		}
	}
	return clonedCommands
}

func cloneReconstructedLogs(logs []ReconstructedLog) []ReconstructedLog {
	if len(logs) == 0 {
		return nil
	}
	clonedLogs := make([]ReconstructedLog, len(logs))
	copy(clonedLogs, logs)
	return clonedLogs
}

func cloneReconstructedEvents(events []ReconstructedEvent) []ReconstructedEvent {
	if len(events) == 0 {
		return nil
	}
	clonedEvents := make([]ReconstructedEvent, len(events))
	for eventIndex, event := range events {
		clonedEvents[eventIndex] = event
		clonedEvents[eventIndex].Data = cloneTelemetryContext(event.Data)
	}
	return clonedEvents
}
