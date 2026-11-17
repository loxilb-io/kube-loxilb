package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type LoxiResponse struct {
	err         error
	statusCode  int
	body        []byte
	contentType string
}

// requireOK - fail a response whose status is not 200, keeping the code and
// loxilb's reason.
//
// Do checks the status of every method but GET, whose callers - the health
// check first - read the code themselves. Any other GET has to ask for this
// before UnMarshal: an error answer with a JSON body otherwise unmarshals into
// the model's zero values without complaint.
func (l *LoxiResponse) requireOK() *LoxiResponse {
	if l.err == nil && l.statusCode != http.StatusOK {
		l.err = &APIError{StatusCode: l.statusCode, Message: resultMessage(l.body, l.statusCode)}
	}

	return l
}

func (l *LoxiResponse) UnMarshal(obj LoxiModel) *LoxiResponse {
	if l.err != nil {
		return l
	}

	switch l.contentType {
	case "application/json":
		l.err = json.Unmarshal(l.body, obj)
	default:
		l.err = fmt.Errorf("not supported content-Type %s", l.contentType)
	}

	return l
}
