//go:build qjsscript

package script

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/script/qjs"
)

// koraShimSource is the JavaScript function that builds the kora global over __kora_host.
// It mirrors the goja binding in api.go; the Go side only marshals.
//
//go:embed kora_shim.js
var koraShimSource string

// koraBridge answers the guest's __kora_host calls by dispatching to koraOps.
type koraBridge struct{ ops *koraOps }

// handle answers one request with {"ok": value} or {"throw": message}. It never panics:
// a malformed request, an unknown op, an op error and an unencodable result all throw.
func (b koraBridge) handle(_ context.Context, raw []byte) []byte {
	out, err := b.dispatch(raw)
	if err != nil {
		return throwJSON(err.Error())
	}
	body, err := json.Marshal(map[string]any{"ok": out})
	if err != nil {
		return throwJSON(fmt.Sprintf("kora: result not encodable: %v", err))
	}
	return body
}

// dispatch decodes one request and runs the op it names.
func (b koraBridge) dispatch(raw []byte) (any, error) {
	v, err := decodeJSValue(string(raw))
	if err != nil {
		return nil, fmt.Errorf("kora: malformed request: %w", err)
	}
	req, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("kora: malformed request: want an object")
	}
	op, _ := req["op"].(string)
	call, ok := koraCalls[op]
	if !ok {
		return nil, fmt.Errorf("kora: unknown operation %q", op)
	}
	args, _ := req["args"].([]any) // a non-array args list reads as empty, so the op's argument check throws
	return call(b.ops, &hostArgs{list: args})
}

// koraCall runs one op. It reads and checks every argument before it calls koraOps.
type koraCall func(o *koraOps, a *hostArgs) (any, error)

// koraCalls maps each op name the shim sends to its koraOps method.
var koraCalls = map[string]koraCall{
	"log": func(o *koraOps, a *hostArgs) (any, error) {
		level, msg := a.str(0), a.str(1)
		if err := a.check(); err != nil {
			return nil, err
		}
		o.log(level, msg)
		return nil, nil
	},
	"getDoc": func(o *koraOps, a *hostArgs) (any, error) {
		doctype, name := a.str(0), a.str(1)
		if err := a.check(); err != nil {
			return nil, err
		}
		return o.getDoc(doctype, name)
	},
	"getList": func(o *koraOps, a *hostArgs) (any, error) {
		doctype, opts := a.str(0), a.obj()
		if err := a.check(); err != nil {
			return nil, err
		}
		return o.getList(doctype, opts)
	},
	"hasDocType": func(o *koraOps, a *hostArgs) (any, error) {
		doctype := a.str(0)
		if err := a.check(); err != nil {
			return nil, err
		}
		return o.hasDocType(doctype), nil
	},
	"saveDoc": func(o *koraOps, a *hostArgs) (any, error) {
		doctype, doc := a.str(0), a.obj()
		if err := a.check(); err != nil {
			return nil, err
		}
		return o.saveDoc(doctype, doc)
	},
	"createDoc": func(o *koraOps, a *hostArgs) (any, error) {
		doctype, doc := a.str(0), a.obj()
		if err := a.check(); err != nil {
			return nil, err
		}
		return o.createDoc(doctype, doc)
	},
	"deleteDoc": func(o *koraOps, a *hostArgs) (any, error) {
		doctype, name := a.str(0), a.str(1)
		if err := a.check(); err != nil {
			return nil, err
		}
		return nil, o.deleteDoc(doctype, name)
	},
	"secret": func(o *koraOps, a *hostArgs) (any, error) {
		key := a.str(0)
		if err := a.check(); err != nil {
			return nil, err
		}
		return o.getSecret(key), nil
	},
	"fetch": func(o *koraOps, a *hostArgs) (any, error) {
		url, opts := a.str(0), a.obj()
		if err := a.check(); err != nil {
			return nil, err
		}
		resp, err := o.fetch(url, opts)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": resp.Status, "statusText": resp.StatusText, "body": string(resp.Body)}, nil
	},
}

// hostArgs reads the arguments of one request. The first wrong argument is recorded
// and reported by check, so an op reads all its arguments before it acts.
type hostArgs struct {
	list []any
	bad  error
}

func (a *hostArgs) arg(i int) any {
	if i < len(a.list) {
		return a.list[i]
	}
	return nil
}

func (a *hostArgs) str(i int) string {
	s, ok := a.arg(i).(string)
	a.want(ok, i, "a string")
	return s
}

// obj reads the object argument, which every op that takes one passes second.
func (a *hostArgs) obj() map[string]any {
	m, ok := a.arg(1).(map[string]any)
	a.want(ok, 1, "an object")
	return m
}

func (a *hostArgs) want(ok bool, i int, kind string) {
	if !ok && a.bad == nil {
		a.bad = fmt.Errorf("kora: argument %d must be %s", i+1, kind)
	}
}

// check returns the first argument error, if any.
func (a *hostArgs) check() error {
	return a.bad
}

// throwJSON is the response that makes the guest throw msg.
func throwJSON(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"throw": msg}) // a string-only map always marshals
	return b
}

// installKora evaluates the shim, which builds the kora global over __kora_host and then
// removes __kora_host from the script's view.
func installKora(ctx context.Context, inst *qjs.Instance, ops *koraOps, req ExecuteRequest) error {
	setup, err := json.Marshal(map[string]any{
		"context": map[string]any{
			"user": req.User, "userRoles": req.UserRoles, "site": req.Site,
			"doctype": req.DocType, "event": string(req.Event),
		},
		"docs":    ops.hasDocs(),
		"secrets": ops.hasSecrets(),
		"http":    ops.hasHTTP(),
	})
	if err != nil {
		return fmt.Errorf("script: encode kora setup: %w", err)
	}
	src := fmt.Sprintf("globalThis.kora = %s(__kora_host, %s); delete globalThis.__kora_host;",
		strings.TrimSpace(koraShimSource), setup)
	return evalSetup(ctx, inst, src)
}
