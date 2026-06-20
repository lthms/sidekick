package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-playground/validator/v10"
	"github.com/sourcegraph/jsonrpc2"
)

type handlers struct {
	m map[string]rpcHandler
}

func new() handlers {
	return handlers{
		m: map[string]rpcHandler{},
	}
}

func set[T any, U any](self *handlers, method string, fn func(context.Context, T, *registry) (U, *jsonrpc2.Error)) {
	self.m[method] = typed(fn)
}

type registerRequest struct {
	Endpoint string `json:"endpoint" validate:"required"`
	PID      int    `JSON:"pid" validate:"required"`
}

type registerResponse struct {
	Ok bool `json:"ok"`
}

func handleRegister(_ context.Context, p registerRequest, reg *registry) (registerResponse, *jsonrpc2.Error) {
	slog.Info("registered nvim RPC endpoint", "endpoint", p.Endpoint)
	reg.remember(p.PID, p.Endpoint)
	return registerResponse{Ok: true}, nil
}

func handleNotifyBufWrite(_ context.Context, p BufWritePostData, reg *registry) (any, *jsonrpc2.Error) {
	reg.notify(p.PID, p)

	return nil, nil
}

func handleRPC(reg *registry) func(http.ResponseWriter, *http.Request) {
	hs := new()
	set(&hs, "register", handleRegister)
	set(&hs, "notifyBufWrite", handleNotifyBufWrite)

	return func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpc2.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPCResponse(w, &jsonrpc2.Response{
				Error: &jsonrpc2.Error{Code: jsonrpc2.CodeParseError, Message: err.Error()},
			})
			return
		}

		result, rpcErr := hs.dispatch(r.Context(), &req, reg)

		if req.Notif {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		resp := &jsonrpc2.Response{ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = &result
		}
		writeRPCResponse(w, resp)
	}
}

type rpcHandler func(context.Context, *jsonrpc2.Request, *registry) (json.RawMessage, *jsonrpc2.Error)

func typed[T any, U any](fn func(context.Context, T, *registry) (U, *jsonrpc2.Error)) rpcHandler {
	return func(ctx context.Context, req *jsonrpc2.Request, reg *registry) (json.RawMessage, *jsonrpc2.Error) {
		p, rpcErr := unmarshalParams[T](req)
		if rpcErr != nil {
			return nil, rpcErr
		}
		res, err := fn(ctx, p, reg)
		if err != nil {
			return nil, err
		}

		raw, marshalErr := json.Marshal(res)
		if marshalErr != nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInternalError, Message: marshalErr.Error()}
		}

		return raw, nil
	}
}

func (self *handlers) dispatch(ctx context.Context, req *jsonrpc2.Request, reg *registry) (json.RawMessage, *jsonrpc2.Error) {
	handler, ok := self.m[req.Method]
	if !ok {
		slog.Info("unknown method", "method", req.Method)
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: "unknown method: " + req.Method}
	}
	return handler(ctx, req, reg)
}

var validate = validator.New(validator.WithRequiredStructEnabled())

func unmarshalParams[T any](req *jsonrpc2.Request) (T, *jsonrpc2.Error) {
	var p T
	if req.Params != nil {
		if err := json.Unmarshal(*req.Params, &p); err != nil {
			return p, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: err.Error()}
		}
	}
	if err := validate.Struct(p); err != nil {
		return p, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: err.Error()}
	}
	return p, nil
}

func writeRPCResponse(w http.ResponseWriter, resp *jsonrpc2.Response) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("write jsonrpc response", "err", err)
	}
}
