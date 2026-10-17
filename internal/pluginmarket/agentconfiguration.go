package pluginmarket

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"
)

// The Go mirror of the desktop's static Agent declaration compiler
// (crates/plugin-config/src/agent.rs). Both implementations are constrained by the same shared
// input/output vectors (crates/plugin-config/tests/fixtures/agent-configuration-contract.json in
// the desktop repository, copied to this package's testdata), so a declaration the marketplace
// sync accepts is exactly one a Node's Rust compiler accepts; neither side invents a second
// business rule.
//
// The compiler is deliberately I/O-free: it validates bytes and reports a bounded error
// vocabulary. It never loads a user's configuration, resolves a connection, or authorizes a
// runtime.

// maxAgentDeclarationBytes bounds one declaration before parsing, mirroring the desktop's
// MAX_DECLARATION_BYTES.
const maxAgentDeclarationBytes = 256 << 10

// maxAgentSettings bounds how many Settings one declaration may render.
const maxAgentSettings = 128

// The declaration schema and model-access contract versions this compiler supports. The desktop
// owns the version domains; this mirror rejects any other version explicitly rather than
// treating it as "no declaration".
const (
	agentDeclarationSchemaVersion   = 2
	agentModelAccessContractVersion = 1
	agentModelCardinalitySingle     = "single"
	settingTitleMaximumCharacters   = 120
	settingDescriptionMaximumRunes  = 2000
	settingIdentifierMaximumBytes   = 64
)

// AgentConfigurationError reports why one Agent declaration failed to compile. Code is the
// stable, cross-language vocabulary the shared contract vectors assert; Reason is a bounded
// human-readable detail that never echoes untrusted declaration content beyond field names.
type AgentConfigurationError struct {
	Code   string
	Reason string
}

func (e *AgentConfigurationError) Error() string {
	if e.Reason == "" {
		return "agent configuration: " + e.Code
	}
	return "agent configuration: " + e.Code + ": " + e.Reason
}

func agentError(code, reason string) *AgentConfigurationError {
	return &AgentConfigurationError{Code: code, Reason: reason}
}

// CompiledAgentConfiguration is the validated static declaration of one Agent release.
type CompiledAgentConfiguration struct {
	SchemaVersion uint32                   `json:"schemaVersion"`
	Settings      []CompiledAgentSetting   `json:"settings"`
	ModelAccess   CompiledAgentModelAccess `json:"modelAccess"`
	Fingerprint   string                   `json:"fingerprint"`
}

// CompiledAgentModelAccess states what the plugin may consume; protocol availability and
// authorization remain host-owned.
type CompiledAgentModelAccess struct {
	ContractVersion  uint32   `json:"contractVersion"`
	Protocols        []string `json:"protocols"`
	ModelCardinality string   `json:"modelCardinality"`
}

// CompiledAgentSetting is one validated Setting declaration; the JSON shape matches the desktop
// compiler's serialization so shared vectors compare structurally.
type CompiledAgentSetting struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	SettingType string `json:"settingType"`
	Required    bool   `json:"required"`
	Order       *int64 `json:"order"`
	Default     any    `json:"default"`
}

// agentJSONObject is one parsed JSON object retaining its member set; duplicate members are
// rejected during parsing, so the member map is exact.
type agentJSONObject struct {
	members map[string]any
}

func (o *agentJSONObject) get(key string) (any, bool) {
	value, ok := o.members[key]
	return value, ok
}

// CompileAgentConfiguration validates one bounded Agent declaration without side effects.
//
// The check order mirrors the desktop compiler exactly, because the shared vectors pin which
// error a multi-problem input produces: size, strict parse, explicit null Settings, structure,
// schema version, contract version, protocols, then Settings validation.
func CompileAgentConfiguration(source []byte) (*CompiledAgentConfiguration, error) {
	if len(source) > maxAgentDeclarationBytes {
		return nil, agentError("too_large", "")
	}
	value, err := parseStrictAgentJSON(source)
	if err != nil {
		if err == errAgentDuplicateKey {
			return nil, agentError("duplicate_key", "")
		}
		return nil, agentError("invalid_json", "")
	}
	root, ok := value.(*agentJSONObject)
	if !ok {
		return nil, agentError("invalid_structure", "declaration must be a JSON object")
	}
	// Option<Value> treats an explicit null like absence in the desktop compiler, but a declared
	// Settings member must be valid, so the null case fails before the structure walk.
	if raw, present := root.get("settings"); present && raw == nil {
		return nil, agentError("invalid_settings", "settings must not be null")
	}
	schemaVersion, err := agentStructure(root)
	if err != nil {
		return nil, err
	}
	if schemaVersion != agentDeclarationSchemaVersion {
		return nil, agentError("unsupported_schema_version", strconv.FormatUint(uint64(schemaVersion), 10))
	}
	modelAccess, err := agentModelAccessOf(root)
	if err != nil {
		return nil, err
	}
	settings, err := compileAgentSettings(root)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(source)
	return &CompiledAgentConfiguration{
		SchemaVersion: agentDeclarationSchemaVersion,
		Settings:      settings,
		ModelAccess:   modelAccess,
		Fingerprint:   hex.EncodeToString(digest[:]),
	}, nil
}

// agentStructure proves the declaration carries exactly the versioned Agent members and returns
// the declared schema version as a validated u32.
func agentStructure(root *agentJSONObject) (uint32, error) {
	for key := range root.members {
		switch key {
		case "schemaVersion", "agent", "settings":
		default:
			return 0, agentError("invalid_structure", "unknown member "+key)
		}
	}
	rawVersion, ok := root.get("schemaVersion")
	if !ok {
		return 0, agentError("invalid_structure", "schemaVersion is required")
	}
	schemaVersion, ok := agentU32(rawVersion)
	if !ok {
		return 0, agentError("invalid_structure", "schemaVersion must be an unsigned integer")
	}
	if _, ok := root.get("agent"); !ok {
		return 0, agentError("invalid_structure", "agent is required")
	}
	return schemaVersion, nil
}

// agentModelAccessOf validates the model-access contribution after the schema version check.
func agentModelAccessOf(root *agentJSONObject) (CompiledAgentModelAccess, error) {
	rawContribution, _ := root.get("agent")
	contribution, ok := rawContribution.(*agentJSONObject)
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "agent must be an object")
	}
	for key := range contribution.members {
		if key != "modelAccess" {
			return CompiledAgentModelAccess{}, agentError("invalid_structure", "unknown member agent."+key)
		}
	}
	rawAccessValue, _ := contribution.get("modelAccess")
	rawAccess, ok := rawAccessValue.(*agentJSONObject)
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "agent.modelAccess must be an object")
	}
	for key := range rawAccess.members {
		switch key {
		case "contractVersion", "protocols", "modelCardinality":
		default:
			return CompiledAgentModelAccess{}, agentError("invalid_structure", "unknown member agent.modelAccess."+key)
		}
	}
	rawContract, ok := rawAccess.get("contractVersion")
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "contractVersion is required")
	}
	contractVersion, ok := agentU32(rawContract)
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "contractVersion must be an unsigned integer")
	}
	if contractVersion != agentModelAccessContractVersion {
		return CompiledAgentModelAccess{}, agentError("unsupported_contract_version", strconv.FormatUint(uint64(contractVersion), 10))
	}
	rawCardinality, ok := rawAccess.get("modelCardinality")
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "modelCardinality is required")
	}
	cardinality, ok := rawCardinality.(string)
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "modelCardinality must be a string")
	}
	// A declaration cannot introduce an unapproved second model by adding a role or fallback;
	// every cardinality other than the approved single-model binding is a structure failure.
	if cardinality != agentModelCardinalitySingle {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "modelCardinality must be single")
	}
	rawProtocols, ok := rawAccess.get("protocols")
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "protocols is required")
	}
	protocolList, ok := rawProtocols.([]any)
	if !ok {
		return CompiledAgentModelAccess{}, agentError("invalid_structure", "protocols must be an array")
	}
	if len(protocolList) == 0 {
		return CompiledAgentModelAccess{}, agentError("empty_protocols", "")
	}
	protocols := make([]string, 0, len(protocolList))
	for _, rawProtocol := range protocolList {
		protocol, ok := rawProtocol.(string)
		if !ok {
			return CompiledAgentModelAccess{}, agentError("invalid_structure", "protocols must be strings")
		}
		// Wire protocol identities, never provider names or CLI-specific aliases.
		switch protocol {
		case "openai-completions", "anthropic-messages":
		default:
			return CompiledAgentModelAccess{}, agentError("unsupported_protocol", "")
		}
		for _, seen := range protocols {
			if seen == protocol {
				return CompiledAgentModelAccess{}, agentError("duplicate_protocol", "")
			}
		}
		protocols = append(protocols, protocol)
	}
	return CompiledAgentModelAccess{
		ContractVersion:  agentModelAccessContractVersion,
		Protocols:        protocols,
		ModelCardinality: agentModelCardinalitySingle,
	}, nil
}

// compileAgentSettings applies the shared scalar Setting rules to the optional Settings member.
//
// The member is optional but cannot be empty: an explicit empty (or null) object fails, exactly
// like the version-one declaration the desktop reuses here. Absent Settings yield no entries.
func compileAgentSettings(root *agentJSONObject) ([]CompiledAgentSetting, error) {
	rawSettings, present := root.get("settings")
	if !present || rawSettings == nil {
		return []CompiledAgentSetting{}, nil
	}
	settings, ok := rawSettings.(*agentJSONObject)
	if !ok {
		return nil, agentError("invalid_settings", "settings must be an object")
	}
	if len(settings.members) == 0 {
		return nil, agentError("invalid_settings", "settings must declare at least one Setting")
	}
	if len(settings.members) > maxAgentSettings {
		return nil, agentError("invalid_settings", "settings exceed the "+strconv.Itoa(maxAgentSettings)+"-Setting limit")
	}
	// BTreeMap iteration in the desktop compiler is key-ordered, so validation failures surface
	// deterministically; the sorted walk keeps this mirror reproducible.
	ids := make([]string, 0, len(settings.members))
	for id := range settings.members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	compiled := make([]CompiledAgentSetting, 0, len(ids))
	for _, id := range ids {
		setting, err := compileAgentSetting(id, settings.members[id])
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, setting)
	}
	// Settings render ordered by their optional order value, then by id; ordered entries sort
	// before unordered ones.
	sort.SliceStable(compiled, func(i, j int) bool {
		left, right := compiled[i], compiled[j]
		if left.Order != nil && right.Order != nil {
			if *left.Order != *right.Order {
				return *left.Order < *right.Order
			}
			return left.ID < right.ID
		}
		if left.Order != nil {
			return true
		}
		if right.Order != nil {
			return false
		}
		return left.ID < right.ID
	})
	return compiled, nil
}

// compileAgentSetting validates one keyed Setting and preserves author text verbatim.
func compileAgentSetting(id string, raw any) (CompiledAgentSetting, error) {
	invalid := func(reason string) (CompiledAgentSetting, error) {
		return CompiledAgentSetting{}, agentError("invalid_settings", reason)
	}
	if !validAgentSettingID(id) {
		return invalid("Setting ID must match ^[a-z][A-Za-z0-9]{0,63}$")
	}
	setting, ok := raw.(*agentJSONObject)
	if !ok {
		return invalid("Setting " + id + " must be an object")
	}
	for key := range setting.members {
		switch key {
		case "type", "title", "description", "required", "order", "default":
		default:
			return invalid("Setting " + id + " declares unknown member " + key)
		}
	}
	rawType, ok := setting.get("type")
	if !ok {
		return invalid("Setting " + id + " must declare a type")
	}
	settingType, ok := rawType.(string)
	if !ok {
		return invalid("Setting " + id + " type must be a string")
	}
	switch settingType {
	case "string", "number", "boolean":
	default:
		return invalid("Setting " + id + " declares unsupported type " + settingType)
	}
	rawTitle, ok := setting.get("title")
	if !ok {
		return invalid("Setting " + id + " must declare a title")
	}
	title, ok := rawTitle.(string)
	if !ok {
		return invalid("Setting " + id + " title must be a string")
	}
	if err := validateAgentText(id, "title", title, settingTitleMaximumCharacters); err != nil {
		return CompiledAgentSetting{}, err
	}
	rawDescription, ok := setting.get("description")
	if !ok {
		return invalid("Setting " + id + " must declare a description")
	}
	description, ok := rawDescription.(string)
	if !ok {
		return invalid("Setting " + id + " description must be a string")
	}
	if err := validateAgentText(id, "description", description, settingDescriptionMaximumRunes); err != nil {
		return CompiledAgentSetting{}, err
	}
	required := false
	if rawRequired, present := setting.get("required"); present {
		value, ok := rawRequired.(bool)
		if !ok {
			return invalid("Setting " + id + " required must be a boolean")
		}
		required = value
	}
	var order *int64
	if rawOrder, present := setting.get("order"); present {
		value, err := agentInteger(rawOrder, id, "order")
		if err != nil {
			return CompiledAgentSetting{}, err
		}
		order = &value
	}
	var defaultValue any
	if rawDefault, present := setting.get("default"); present {
		if err := validateAgentDefault(id, settingType, rawDefault); err != nil {
			return CompiledAgentSetting{}, err
		}
		defaultValue = rawDefault
	}
	return CompiledAgentSetting{
		ID:          id,
		Title:       title,
		Description: description,
		SettingType: settingType,
		Required:    required,
		Order:       order,
		Default:     defaultValue,
	}, nil
}

// validateAgentText applies the shared non-blank and character-count limits.
func validateAgentText(id, field, value string, maximumCharacters int) error {
	if len(bytes.TrimSpace([]byte(value))) == 0 {
		return agentError("invalid_settings", "Setting "+id+" "+field+" must not be blank")
	}
	if utf8.RuneCountInString(value) > maximumCharacters {
		return agentError("invalid_settings", "Setting "+id+" "+field+" exceeds "+strconv.Itoa(maximumCharacters)+" characters")
	}
	return nil
}

// validateAgentDefault accepts a scalar only when it matches the declared type; an explicit null
// default is present-but-mismatched, not absent.
func validateAgentDefault(id, settingType string, value any) error {
	mismatch := func(expected string) error {
		return agentError("invalid_settings", "Setting "+id+" default must be "+expected)
	}
	switch settingType {
	case "string":
		if _, ok := value.(string); !ok {
			return mismatch("a string")
		}
	case "number":
		if _, ok := value.(jsonNumber); !ok {
			return mismatch("a finite number")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return mismatch("a boolean")
		}
	}
	return nil
}

// validAgentSettingID applies the ASCII identifier grammar without a regular expression.
func validAgentSettingID(id string) bool {
	if id == "" || len(id) > settingIdentifierMaximumBytes {
		return false
	}
	for index, char := range []byte(id) {
		switch {
		case char >= 'a' && char <= 'z':
		case index > 0 && char >= 'A' && char <= 'Z':
		case index > 0 && char >= '0' && char <= '9':
		default:
			return false
		}
	}
	return true
}

// agentU32 accepts a JSON number that fits a u32, mirroring serde's u32 deserialization for the
// version fields.
func agentU32(value any) (uint32, bool) {
	number, ok := value.(jsonNumber)
	if !ok {
		return 0, false
	}
	integral, err := strconv.ParseUint(number.text, 10, 64)
	if err != nil || integral > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(integral), true
}

// agentInteger accepts a JSON number that fits the desktop's i64 order field.
func agentInteger(value any, id, field string) (int64, error) {
	number, ok := value.(jsonNumber)
	if !ok {
		return 0, agentError("invalid_settings", "Setting "+id+" "+field+" must be an integer")
	}
	integral, err := strconv.ParseInt(number.text, 10, 64)
	if err != nil {
		return 0, agentError("invalid_settings", "Setting "+id+" "+field+" must be an integer")
	}
	return integral, nil
}

// errAgentDuplicateKey marks a duplicate object member at any nesting depth.
var errAgentDuplicateKey = fmt.Errorf("duplicate JSON key")

// jsonNumber preserves one JSON number literal; the desktop keeps arbitrary-precision integers
// and finite floats, so out-of-range literals fail at parse time exactly like serde_json.
type jsonNumber struct {
	text string
}

// MarshalJSON renders the number's original literal so compiled output keeps JSON numbers as
// numbers, never as re-formatted strings.
func (n jsonNumber) MarshalJSON() ([]byte, error) {
	return []byte(n.text), nil
}

// parseStrictAgentJSON decodes exactly one JSON document, rejecting duplicate object members,
// trailing content, invalid UTF-8, and numbers no finite float can represent.
func parseStrictAgentJSON(source []byte) (any, error) {
	if !utf8.Valid(source) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	value, err := parseAgentValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing content")
		}
		return nil, err
	}
	return value, nil
}

// parseAgentValue consumes one complete value from the token stream.
func parseAgentValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			return parseAgentObject(decoder)
		case '[':
			return parseAgentArray(decoder)
		default:
			// A closing delimiter as a value start is a syntax error the decoder already
			// tokenized; surface it as invalid JSON rather than a typed value.
			return nil, fmt.Errorf("unexpected delimiter %v", rune(value))
		}
	case json.Number:
		// serde_json rejects literals no finite float can hold; strconv reports those as
		// out-of-range, so the same literals fail here at parse time.
		if _, err := strconv.ParseFloat(value.String(), 64); err != nil {
			return nil, err
		}
		return jsonNumber{text: value.String()}, nil
	default:
		return token, nil
	}
}

// parseAgentObject builds one duplicate-free object, failing on the first repeated member.
func parseAgentObject(decoder *json.Decoder) (*agentJSONObject, error) {
	object := &agentJSONObject{members: map[string]any{}}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("object member name must be a string")
		}
		if _, exists := object.members[key]; exists {
			return nil, errAgentDuplicateKey
		}
		value, err := parseAgentValue(decoder)
		if err != nil {
			return nil, err
		}
		object.members[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return object, nil
}

// parseAgentArray builds one array of parsed values.
func parseAgentArray(decoder *json.Decoder) ([]any, error) {
	array := []any{}
	for decoder.More() {
		value, err := parseAgentValue(decoder)
		if err != nil {
			return nil, err
		}
		array = append(array, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return array, nil
}
