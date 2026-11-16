package runner

import "errors"

const (
	ResourceLimitsRequired = "required"
	ResourceLimitsDisabled = "disabled"
	resourceLimitsWarning  = "development mode: CPU, memory and PID hard limits are disabled; tasks may exhaust host resources"
)

func normalizeResourceLimits(mode string) (string, error) {
	if mode == "" {
		return ResourceLimitsRequired, nil
	}
	if mode != ResourceLimitsRequired && mode != ResourceLimitsDisabled {
		return "", errors.New("RUNNER_RESOURCE_LIMITS must be required or disabled")
	}
	return mode, nil
}

func resourceWarning(mode string) string {
	if mode == ResourceLimitsDisabled {
		return resourceLimitsWarning
	}
	return ""
}
