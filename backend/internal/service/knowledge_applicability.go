package service

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Knowledge applicability is deterministic and is checked both before model
// input and before sending. cli_version accepts a complete SemVer 2.0 version
// (optional v prefix), or >, >=, <, <= comparisons joined by spaces/commas (AND).
// Empty means unrestricted. OR, wildcards, partial versions, ^ and ~ are rejected.
// Build metadata does not affect precedence (https://semver.org/). Prerelease
// case versions require a constraint explicitly mentioning a prerelease with the
// same major.minor.patch; a broad stable range never implicitly opts into RCs.
//
// environment accepts slash-separated AND conditions, at most one per dimension:
// OS (macOS/darwin, Windows, Linux), deployment (prod/production/生产, BOE,
// test/testing/测试, dev/development/开发, staging/预发), and auth mode (keyless,
// client_assertion, DPoP, OAuth, device_flow, app_secret, PAT, api_key).
// Example: "macOS/BOE/keyless". Unsupported or contradictory conditions fail
// closed at save and read. Explicit case.environment.cli_version/os/environment/
// auth_mode wins. Text extraction only accepts explicitly labelled CLI versions,
// unique explicit OS/deployment terms, and labelled auth mode. Ambiguity and
// missing required dimensions request clarification; the model cannot override it.
type semanticVersion struct {
	core [3]string
	pre  []string
}
type versionConstraint struct {
	op      string
	version semanticVersion
}

var semanticPattern = regexp.MustCompile(`^[vV]?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
var numericIdentifier = regexp.MustCompile(`^[0-9]+$`)

func parseSemantic(s string) (semanticVersion, error) {
	var out semanticVersion
	if len(s) > 256 {
		return out, fmt.Errorf("version too long")
	}
	match := semanticPattern.FindStringSubmatch(s)
	if match == nil {
		return out, fmt.Errorf("complete SemVer required")
	}
	copy(out.core[:], match[1:4])
	if match[4] != "" {
		out.pre = strings.Split(match[4], ".")
		for _, id := range out.pre {
			if numericIdentifier.MatchString(id) && len(id) > 1 && id[0] == '0' {
				return out, fmt.Errorf("numeric prerelease cannot have leading zeros")
			}
		}
	}
	return out, nil
}
func compareNumeric(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}
func compareSemantic(a, b semanticVersion) int {
	for i := range a.core {
		if n := compareNumeric(a.core[i], b.core[i]); n != 0 {
			return n
		}
	}
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return 0
	}
	if len(a.pre) == 0 {
		return 1
	}
	if len(b.pre) == 0 {
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		nx, ny := numericIdentifier.MatchString(x), numericIdentifier.MatchString(y)
		n := 0
		switch {
		case nx && ny:
			n = compareNumeric(x, y)
		case nx:
			n = -1
		case ny:
			n = 1
		default:
			n = strings.Compare(x, y)
		}
		if n != 0 {
			return n
		}
	}
	if len(a.pre) < len(b.pre) {
		return -1
	}
	if len(a.pre) > len(b.pre) {
		return 1
	}
	return 0
}
func parseVersionConstraints(raw string) ([]versionConstraint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 1024 {
		return nil, fmt.Errorf("range too long")
	}
	if strings.HasPrefix(raw, ",") || strings.HasSuffix(raw, ",") || strings.Contains(raw, ",,") {
		return nil, fmt.Errorf("empty comparison")
	}
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == "" {
			return nil, fmt.Errorf("empty comparison")
		}
	}
	fields := strings.Fields(strings.ReplaceAll(raw, ",", " "))
	out := []versionConstraint{}
	for i := 0; i < len(fields); i++ {
		token := fields[i]
		op := "="
		for _, candidate := range []string{">=", "<=", ">", "<"} {
			if strings.HasPrefix(token, candidate) {
				op = candidate
				token = strings.TrimPrefix(token, candidate)
				break
			}
		}
		if token == "" {
			i++
			if i >= len(fields) {
				return nil, fmt.Errorf("comparison without version")
			}
			token = fields[i]
		}
		v, e := parseSemantic(token)
		if e != nil {
			return nil, e
		}
		out = append(out, versionConstraint{op, v})
		if len(out) > 16 {
			return nil, fmt.Errorf("too many comparisons")
		}
	}
	return out, nil
}
func versionMatches(v semanticVersion, conditions []versionConstraint) bool {
	if len(conditions) == 0 {
		return true
	}
	if len(v.pre) > 0 {
		explicit := false
		for _, c := range conditions {
			if c.version.core == v.core && len(c.version.pre) > 0 {
				explicit = true
			}
		}
		if !explicit {
			return false
		}
	}
	for _, c := range conditions {
		n := compareSemantic(v, c.version)
		switch c.op {
		case "=":
			if n != 0 {
				return false
			}
		case ">":
			if n <= 0 {
				return false
			}
		case ">=":
			if n < 0 {
				return false
			}
		case "<":
			if n >= 0 {
				return false
			}
		case "<=":
			if n > 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

var applicabilityAliases = map[string]map[string]string{
	"os":          {"macos": "macos", "mac os": "macos", "mac os x": "macos", "osx": "macos", "darwin": "macos", "windows": "windows", "win32": "windows", "win64": "windows", "linux": "linux"},
	"environment": {"prod": "prod", "production": "prod", "生产": "prod", "生产环境": "prod", "线上": "prod", "boe": "boe", "test": "test", "testing": "test", "测试": "test", "测试环境": "test", "dev": "dev", "development": "dev", "开发": "dev", "开发环境": "dev", "staging": "staging", "stage": "staging", "预发": "staging", "预发环境": "staging"},
	"auth_mode":   {"keyless": "keyless", "client_assertion": "client_assertion", "client assertion": "client_assertion", "client-assertion": "client_assertion", "dpop": "dpop", "oauth": "oauth", "oauth2": "oauth", "device_flow": "device_flow", "device flow": "device_flow", "device_code": "device_flow", "app_secret": "app_secret", "appsecret": "app_secret", "client_secret": "app_secret", "pat": "pat", "api_key": "api_key"},
}

func normalizedCondition(dimension, value string) (string, bool) {
	canonical, ok := applicabilityAliases[dimension][strings.ToLower(strings.TrimSpace(value))]
	return canonical, ok
}
func parseEnvironmentConditions(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if len(raw) > 256 {
		return nil, fmt.Errorf("environment too long")
	}
	for _, part := range strings.Split(raw, "/") {
		found := false
		for _, dimension := range []string{"os", "environment", "auth_mode"} {
			if value, ok := normalizedCondition(dimension, part); ok {
				if old, exists := out[dimension]; exists && old != value {
					return nil, fmt.Errorf("contradictory environment conditions")
				}
				out[dimension] = value
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown environment condition")
		}
	}
	return out, nil
}
func validateKnowledgeApplicability(k Data) error {
	for _, key := range []string{"cli_version", "environment"} {
		if value, exists := k[key]; exists {
			if _, ok := value.(string); !ok {
				return Fail(422, "invalid_applicability", "知识版本和环境条件必须为字符串")
			}
		}
	}
	if _, e := parseVersionConstraints(Str(k, "cli_version")); e != nil {
		return Fail(422, "invalid_applicability", "CLI 版本只支持完整 SemVer（可带 v）、>、>=、<、<=；空格或逗号表示同时满足，暂不支持 OR、通配符、^、~")
	}
	if _, e := parseEnvironmentConditions(Str(k, "environment")); e != nil {
		return Fail(422, "invalid_applicability", "环境条件用 / 连接，每类最多一个：操作系统、部署环境、认证方式，例如 macOS/BOE/keyless；未知条件不能发布")
	}
	return nil
}

type applicabilityContext struct {
	values    map[string]string
	uncertain map[string]bool
}

var explicitCLIVersion = regexp.MustCompile(`(?i)(?:\b(?:lark[- ]cli|cli)[ \t]*(?:version|版本))[ \t]*(?:[:：=]|is\b|是|为)?[ \t]*([vV]?[0-9][0-9A-Za-z.+-]*)`)
var explicitAuthMode = regexp.MustCompile(`(?i)(?:\bauth[ _-]*mode|认证方式|认证模式)[ \t]*(?:[:：=]|is\b|是|为)[ \t]*([A-Za-z_][A-Za-z_-]*(?:[ ](?:flow|assertion))?)`)
var explicitDeployment = regexp.MustCompile(`(?i)(?:\benvironment|\benv|部署环境|环境)[ \t]*(?:[:：=]|is\b|是|为)[ \t]*(production|prod|boe|test|testing|dev|development|stage|staging|生产|线上|测试|开发|预发)(?:\b|$)`)
var deploymentSuffix = regexp.MustCompile(`(?i)\b(test|testing|dev|development|stage)[ \t]*(?:environment\b|环境)`)
var deploymentAlternatives = regexp.MustCompile(`(?i)\b(production|prod|boe|test|testing|dev|development|stage|staging)[ \t]*/[ \t]*(production|prod|boe|test|testing|dev|development|stage|staging)\b`)
var followingAlternative = regexp.MustCompile(`(?i)^[ \t]*(?:/|\bor\b|或|\|)`)
var contextTerms = map[string]*regexp.Regexp{
	"os":          regexp.MustCompile(`(?i)\b(?:mac[ ]os(?:[ ]x)?|macos|osx|darwin|windows|win32|win64|linux)\b`),
	"environment": regexp.MustCompile(`(?i)\b(?:production|prod|boe|staging)\b|生产环境|生产|线上|测试环境|开发环境|预发环境|预发`),
}

func negatedAt(text string, at int) bool {
	start := at - 40
	if start < 0 {
		start = 0
	}
	prefix := strings.ToLower(text[start:at])
	for _, term := range []string{"不是", "并非", "不在", "非", "not ", "not on ", "without ", "isn't "} {
		if strings.Contains(prefix, term) {
			return true
		}
	}
	return false
}
func caseApplicability(c Data) applicabilityContext {
	result := applicabilityContext{values: map[string]string{}, uncertain: map[string]bool{}}
	env := Object(c, "environment")
	text := Str(c, "title") + "\n" + Str(c, "description")
	for _, dimension := range []string{"cli_version", "os", "environment", "auth_mode"} {
		if raw, exists := env[dimension]; exists {
			if value, ok := raw.(string); ok && strings.TrimSpace(value) != "" {
				value = strings.TrimSpace(value)
				if dimension == "cli_version" {
					if v, e := parseSemantic(value); e == nil {
						result.values[dimension] = semanticString(v)
					} else {
						result.uncertain[dimension] = true
					}
				} else if normalized, ok := normalizedCondition(dimension, value); ok {
					result.values[dimension] = normalized
				} else {
					result.uncertain[dimension] = true
				}
				continue
			} else if !ok {
				result.uncertain[dimension] = true
				continue
			}
		}
		found := map[string]bool{}
		uncertain := false
		switch dimension {
		case "cli_version":
			for _, m := range explicitCLIVersion.FindAllStringSubmatchIndex(text, -1) {
				raw := strings.TrimRight(text[m[2]:m[3]], ".")
				v, e := parseSemantic(raw)
				if e != nil || negatedAt(text, m[0]) || followingAlternative.MatchString(text[m[3]:]) {
					uncertain = true
				} else {
					found[semanticString(v)] = true
				}
			}
		case "auth_mode":
			for _, m := range explicitAuthMode.FindAllStringSubmatchIndex(text, -1) {
				value, ok := normalizedCondition(dimension, text[m[2]:m[3]])
				if !ok || negatedAt(text, m[0]) || followingAlternative.MatchString(text[m[3]:]) {
					uncertain = true
				} else {
					found[value] = true
				}
			}
		default:
			for _, m := range contextTerms[dimension].FindAllStringIndex(text, -1) {
				value, ok := normalizedCondition(dimension, text[m[0]:m[1]])
				if !ok || negatedAt(text, m[0]) {
					uncertain = true
				} else {
					found[value] = true
				}
			}
			if dimension == "environment" {
				for _, pattern := range []*regexp.Regexp{explicitDeployment, deploymentSuffix} {
					for _, m := range pattern.FindAllStringSubmatchIndex(text, -1) {
						value, ok := normalizedCondition(dimension, text[m[2]:m[3]])
						if !ok || negatedAt(text, m[0]) {
							uncertain = true
						} else {
							found[value] = true
						}
					}
				}
				for _, m := range deploymentAlternatives.FindAllStringSubmatch(text, -1) {
					for _, raw := range m[1:] {
						value, _ := normalizedCondition(dimension, raw)
						found[value] = true
					}
				}
			}
		}
		if len(found) > 1 || uncertain {
			result.uncertain[dimension] = true
		} else {
			for value := range found {
				result.values[dimension] = value
			}
		}
	}
	return result
}
func semanticString(v semanticVersion) string {
	s := strings.Join(v.core[:], ".")
	if len(v.pre) > 0 {
		s += "-" + strings.Join(v.pre, ".")
	}
	return s
}

var dimensionLabels = map[string]string{"cli_version": "CLI 版本（完整 x.y.z）", "os": "操作系统", "environment": "部署环境", "auth_mode": "认证方式"}

func knowledgeApplicability(k, c Data) error {
	constraints, e := parseVersionConstraints(Str(k, "cli_version"))
	if e != nil {
		return Fail(409, "knowledge_applicability", "知识版本范围不受支持，需要维护者修订")
	}
	conditions, e := parseEnvironmentConditions(Str(k, "environment"))
	if e != nil {
		return Fail(409, "knowledge_applicability", "知识环境条件不受支持，需要维护者修订")
	}
	context := caseApplicability(c)
	required := []string{}
	if len(constraints) > 0 {
		required = append(required, "cli_version")
	}
	for dimension := range conditions {
		required = append(required, dimension)
	}
	sort.Strings(required)
	missing := []string{}
	for _, dimension := range required {
		if context.uncertain[dimension] || context.values[dimension] == "" {
			missing = append(missing, dimensionLabels[dimension])
		}
	}
	if len(missing) > 0 {
		return Fail(409, "knowledge_applicability", "请补充或确认"+strings.Join(missing, "、")+"；未明确前不能采用此知识")
	}
	if len(constraints) > 0 {
		v, e := parseSemantic(context.values["cli_version"])
		if e != nil || !versionMatches(v, constraints) {
			return Fail(409, "knowledge_applicability", "此知识不适用于当前 CLI 版本，请选择匹配版本的知识或转人工")
		}
	}
	for dimension, expected := range conditions {
		if context.values[dimension] != expected {
			return Fail(409, "knowledge_applicability", "此知识的"+dimensionLabels[dimension]+"与当前问题不匹配，请转人工核对")
		}
	}
	return nil
}
