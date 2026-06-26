package script

import (
	"fmt"

	"github.com/dop251/goja"
)

// koraAPI is the JavaScript-accessible API injected into every script.
// It bridges script calls to the KoraProvider.
type koraAPI struct {
	req    ExecuteRequest
	runner *EmbeddedRunner
	logs   []LogEntry
}

// buildObject constructs the JavaScript 'kora' global object.
func (api *koraAPI) buildObject(vm *goja.Runtime) goja.Value {
	obj := vm.NewObject()

	// kora.log — logging
	logObj := vm.NewObject()
	logObj.Set("debug", func(call goja.FunctionCall) goja.Value { api.log("debug", call); return goja.Undefined() })
	logObj.Set("info", func(call goja.FunctionCall) goja.Value { api.log("info", call); return goja.Undefined() })
	logObj.Set("warn", func(call goja.FunctionCall) goja.Value { api.log("warn", call); return goja.Undefined() })
	logObj.Set("error", func(call goja.FunctionCall) goja.Value { api.log("error", call); return goja.Undefined() })
	obj.Set("log", logObj)

	// kora.context — request context
	ctxObj := vm.NewObject()
	ctxObj.Set("user", api.req.User)
	ctxObj.Set("userRoles", api.req.UserRoles)
	ctxObj.Set("site", api.req.Site)
	ctxObj.Set("doctype", api.req.DocType)
	ctxObj.Set("event", string(api.req.Event))
	obj.Set("context", ctxObj)

	// kora.now() — current timestamp via JS Date
	obj.Set("now", func(call goja.FunctionCall) goja.Value {
		return vm.Get("Date")
	})

	// kora.getDoc(doctype, name) → document or null
	obj.Set("getDoc", func(call goja.FunctionCall) goja.Value {
		if api.req.Provider == nil {
			return goja.Null()
		}
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kora.getDoc requires 2 arguments: doctype, name"))
		}
		doctype := call.Arguments[0].String()
		name := call.Arguments[1].String()
		doc, err := api.req.Provider.GetDoc(doctype, name)
		if err != nil {
			api.logMsg("warn", fmt.Sprintf("kora.getDoc(%q, %q) error: %v", doctype, name, err))
			return goja.Null()
		}
		if doc == nil {
			return goja.Null()
		}
		return vm.ToValue(doc)
	})

	// kora.getList(doctype, {filters, orderBy, limit, offset}) → array
	obj.Set("getList", func(call goja.FunctionCall) goja.Value {
		if api.req.Provider == nil {
			return vm.ToValue([]any{})
		}
		if len(call.Arguments) < 1 {
			panic(vm.ToValue("kora.getList requires at least 1 argument: doctype"))
		}
		doctype := call.Arguments[0].String()
		opts := map[string]any{}
		if len(call.Arguments) > 1 {
			if o, ok := call.Arguments[1].Export().(map[string]any); ok {
				opts = o
			}
		}
		filters, _ := opts["filters"].(map[string]any)
		orderBy, _ := opts["orderBy"].(string)
		limit := toInt(opts["limit"], 50)
		offset := toInt(opts["offset"], 0)

		docs, err := api.req.Provider.GetList(doctype, filters, orderBy, limit, offset)
		if err != nil {
			api.logMsg("warn", fmt.Sprintf("kora.getList(%q) error: %v", doctype, err))
			return vm.ToValue([]any{})
		}
		return vm.ToValue(docs)
	})

	// kora.saveDoc(doctype, doc) → document
	obj.Set("saveDoc", func(call goja.FunctionCall) goja.Value {
		if api.req.Provider == nil {
			return goja.Null()
		}
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kora.saveDoc requires 2 arguments: doctype, doc"))
		}
		doctype := call.Arguments[0].String()
		doc, ok := call.Arguments[1].Export().(map[string]any)
		if !ok {
			panic(vm.ToValue("kora.saveDoc: second argument must be an object"))
		}
		if err := api.req.Provider.SaveDoc(doctype, doc, api.req.User); err != nil {
			panic(vm.ToValue(fmt.Sprintf("kora.saveDoc(%q) error: %v", doctype, err)))
		}
		return vm.ToValue(doc)
	})

	// kora.createDoc(doctype, doc) → document
	obj.Set("createDoc", func(call goja.FunctionCall) goja.Value {
		if api.req.Provider == nil {
			return goja.Null()
		}
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kora.createDoc requires 2 arguments: doctype, doc"))
		}
		doctype := call.Arguments[0].String()
		doc, ok := call.Arguments[1].Export().(map[string]any)
		if !ok {
			panic(vm.ToValue("kora.createDoc: second argument must be an object"))
		}
		created, err := api.req.Provider.CreateDoc(doctype, doc, api.req.User, api.req.User)
		if err != nil {
			panic(vm.ToValue(fmt.Sprintf("kora.createDoc(%q) error: %v", doctype, err)))
		}
		return vm.ToValue(created)
	})

	// kora.deleteDoc(doctype, name) → void
	obj.Set("deleteDoc", func(call goja.FunctionCall) goja.Value {
		if api.req.Provider == nil {
			return goja.Undefined()
		}
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kora.deleteDoc requires 2 arguments: doctype, name"))
		}
		doctype := call.Arguments[0].String()
		name := call.Arguments[1].String()
		if err := api.req.Provider.DeleteDoc(doctype, name); err != nil {
			panic(vm.ToValue(fmt.Sprintf("kora.deleteDoc(%q, %q) error: %v", doctype, name, err)))
		}
		return goja.Undefined()
	})

	// kora.secrets.get(key) → string
	secretsObj := vm.NewObject()
	secretsObj.Set("get", func(call goja.FunctionCall) goja.Value {
		if api.req.Provider == nil {
			return goja.Null()
		}
		if len(call.Arguments) < 1 {
			panic(vm.ToValue("kora.secrets.get requires 1 argument: key"))
		}
		val, err := api.req.Provider.GetSecret(call.Arguments[0].String())
		if err != nil {
			api.logMsg("warn", fmt.Sprintf("kora.secrets.get error: %v", err))
			return goja.Null()
		}
		return vm.ToValue(val)
	})
	obj.Set("secrets", secretsObj)

	// kora.http — external HTTP requests
	httpObj := vm.NewObject()
	httpObj.Set("fetch", func(call goja.FunctionCall) goja.Value {
		if api.req.Provider == nil {
			panic(vm.ToValue("kora.http.fetch: provider not available"))
		}
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kora.http.fetch requires 2 arguments: url, options"))
		}
		url := call.Arguments[0].String()
		opts, ok := call.Arguments[1].Export().(map[string]any)
		if !ok {
			panic(vm.ToValue("kora.http.fetch: second argument must be an options object"))
		}

		method, _ := opts["method"].(string)
		if method == "" {
			method = "GET"
		}
		headers, _ := opts["headers"].(map[string]any)
		hdr := make(map[string]string)
		for k, v := range headers {
			hdr[k] = fmt.Sprint(v)
		}
		body, _ := opts["body"].(string)

		resp, err := api.req.Provider.DoHTTP(&HTTPRequest{
			Method:  method,
			URL:     url,
			Headers: hdr,
			Body:    body,
		})
		if err != nil {
			panic(vm.ToValue(fmt.Sprintf("kora.http.fetch error: %v", err)))
		}

		// Return a response-like object.
		respObj := vm.NewObject()
		respObj.Set("status", resp.Status)
		respObj.Set("statusText", resp.StatusText)
		respObj.Set("ok", resp.Status >= 200 && resp.Status < 300)
		respObj.Set("body", string(resp.Body))

		// json() method
		respObj.Set("json", func() goja.Value {
			// Simple JSON parse — for production, use a proper parser.
			// The caller should catch errors if the body isn't valid JSON.
			val, err := vm.RunString("JSON.parse")
			if err != nil {
				panic(vm.ToValue("failed to parse JSON"))
			}
			if fn, ok := goja.AssertFunction(val); ok {
				result, err := fn(goja.Undefined(), vm.ToValue(string(resp.Body)))
				if err != nil {
					panic(vm.ToValue("JSON parse error: " + err.Error()))
				}
				return result
			}
			return goja.Null()
		})
		return respObj
	})
	obj.Set("http", httpObj)

	return obj
}

func (api *koraAPI) log(level string, call goja.FunctionCall) {
	msg := ""
	if len(call.Arguments) > 0 {
		msg = call.Arguments[0].String()
	}
	api.logMsg(level, msg)
}

func (api *koraAPI) logMsg(level, msg string) {
	api.logs = append(api.logs, LogEntry{Level: level, Message: msg})
}

func toInt(v any, defaultVal int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return defaultVal
}
