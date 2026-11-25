package retrieval

import "context"

func route(response *Response, names []string, explicit bool, capability func(string) (supported, available bool)) *Error {
	operation := *response.Operation
	executions := make([]Execution, 0, len(names))
	available := false
	for _, name := range names {
		supported, ready := capability(name)
		if explicit && !supported {
			return &Error{Code: "unsupported_capability", Message: "the selected provider does not support " + operation + " in this build"}
		}
		if explicit && !ready {
			return &Error{Code: "missing_credentials", Message: "the selected provider has no API key"}
		}
		execution := Execution{Provider: name}
		if !ready {
			reason := "unsupported_capability"
			execution.Status, execution.Reason = "skipped", &reason
		}
		executions = append(executions, execution)
		available = available || ready
	}
	response.Executions = executions
	if !available {
		return &Error{Code: "no_provider_available", Message: "no configured provider supports " + operation}
	}
	return nil
}

func interruption(cause error) (string, *Error) {
	switch cause {
	case errProviderTimeout:
		return "timeout", &Error{Code: "provider_timeout", Message: "provider request timed out"}
	case errCommandTimeout, context.DeadlineExceeded:
		return "timeout", &Error{Code: "command_timeout", Message: "command timed out"}
	default:
		return "cancelled", &Error{Code: "user_cancelled", Message: "command was cancelled"}
	}
}

func interruptedExecution(execution *Execution, cause error) {
	execution.Status, execution.Error = interruption(cause)
	execution.Reason = &execution.Error.Code
}

func failedExecution(execution *Execution, issue *Error) {
	execution.Status, execution.Error = "error", issue
	if issue.Code == "provider_timeout" {
		execution.Status, execution.Reason = "timeout", &issue.Code
	}
}

func fail(response Response, status int, code, message string) (Response, int) {
	response.Outcome = "error"
	response.Error = &Error{Code: code, Message: message}
	return response, status
}
