// Package tdjson bridges arbitrary {method, params} JSON to the TDLib native
// JSON interface via go-tdlib, without per-method typed wrappers. This is the
// core of the gateway's dynamic dispatch: any td_api method is resolved by
// name, never hand-declared.
package tdjson

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zelenin/go-tdlib/client"
)

// rawRequest satisfies client.Request but marshals to an arbitrary td_api
// object: {"@type": method, "@extra": extra, ...params}. go-tdlib's Execute
// and JsonClient.Send call json.Marshal on the Request, so controlling
// MarshalJSON is all it takes to send any method.
type rawRequest struct {
	method string
	params map[string]json.RawMessage
	extra  string
}

func (r *rawRequest) GetFunctionName() string { return r.method }
func (r *rawRequest) SetExtra(e string)        { r.extra = e }
func (r *rawRequest) GetExtra() string         { return r.extra }
func (r *rawRequest) SetType(string)           {} // @type is emitted from method
func (r *rawRequest) GetType() string          { return r.method }

func (r *rawRequest) MarshalJSON() ([]byte, error) {
	obj := make(map[string]json.RawMessage, len(r.params)+2)
	for k, v := range r.params {
		obj[k] = v
	}
	t, _ := json.Marshal(r.method)
	obj["@type"] = t // method wins over any "@type" in params
	if r.extra != "" {
		e, _ := json.Marshal(r.extra)
		obj["@extra"] = e
	}
	return json.Marshal(obj)
}

// ExecuteSync resolves and runs a synchronous td_api method via td_execute.
// params must be a JSON object or null. Returns the raw td_api result JSON.
// Only the ~28 synchronous methods work here; async methods need a live
// client (added with sessions). This is the pre-auth seed of /call.
func ExecuteSync(method string, params json.RawMessage) (json.RawMessage, error) {
	req := &rawRequest{method: method}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &req.params); err != nil {
			return nil, fmt.Errorf("params must be a JSON object: %w", err)
		}
	}

	resp, err := client.Execute(req)
	if err != nil {
		return nil, err
	}
	if resp.MetaType == "error" {
		return nil, fmt.Errorf("tdlib: %s", string(resp.Data))
	}
	return resp.Data, nil
}

// Call resolves and runs any td_api method on an authorized client via
// td_send, awaiting the matching @extra response. This is the async path
// behind /v1/{user|bot}/{id}/call. params must be a JSON object or null.
func Call(ctx context.Context, cl *client.Client, method string, params json.RawMessage) (json.RawMessage, error) {
	req := &rawRequest{method: method}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &req.params); err != nil {
			return nil, fmt.Errorf("params must be a JSON object: %w", err)
		}
	}

	resp, err := cl.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.MetaType == "error" {
		return nil, fmt.Errorf("tdlib: %s", string(resp.Data))
	}
	return resp.Data, nil
}
