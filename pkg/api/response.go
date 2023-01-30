package api

import (
	"encoding/json"
	"fmt"
)

type LoxiResponse struct {
	err         error
	statusCode  int
	body        []byte
	contentType string
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
