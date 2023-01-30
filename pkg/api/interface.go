package api

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"strings"
)

type LoxiAPI interface {
	Get(context.Context, string) (LoxiModel, error)
	List(context.Context) (LoxiModel, error)
	Create(context.Context, LoxiModel) error
	Delete(context.Context, LoxiModel) error
}

type APICommonFunc struct{}

func (a *APICommonFunc) MakeDeletedSubResource(deleteKey []string, LbModel LoxiModel) (string, error) {
	deleteKeyType := LbModel.GetKeyStruct()
	if deleteKeyType == nil {
		return "", fmt.Errorf("struct %s has no key", reflect.ValueOf(LbModel).Elem().Type().Name())
	}
	e := reflect.ValueOf(deleteKeyType).Elem()

	var subResource []string

	for _, key := range deleteKey {
		for i := 0; i < e.NumField(); i++ {
			v, ok := e.Type().Field(i).Tag.Lookup("key")
			if ok {
				if strings.Compare(key, v) == 0 {
					value := e.Field(i).Interface()
					subResource = append(subResource, key, fmt.Sprintf("%v", value))
					break
				}
			}
		}
	}

	if len(subResource) < len(deleteKey)*2 {
		return "", fmt.Errorf("struct %s have not enough keys", e.Type().Name())
	}

	return path.Join(subResource...), nil
}

func (a *APICommonFunc) MakeQueryParam(LbModel LoxiModel) (map[string]string, error) {
	keyType := LbModel.GetKeyStruct()
	if keyType == nil {
		return nil, fmt.Errorf("struct %s has no key", reflect.ValueOf(LbModel).Elem().Type().Name())
	}

	e := reflect.ValueOf(keyType).Elem()
	result := make(map[string]string)

	for i := 0; i < e.NumField(); i++ {
		v, ok := e.Type().Field(i).Tag.Lookup("options")
		if ok {
			result[v] = fmt.Sprintf("%v", e.Field(i).Interface())
		}
	}

	return result, nil
}
