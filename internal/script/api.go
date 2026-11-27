//go:build gojascript

package script

import "github.com/dop251/goja"

// koraAPI is the JavaScript-accessible 'kora' binding injected into every script.
// It checks arguments and converts JS values; the kora.* semantics live in koraOps.
type koraAPI struct {
	req ExecuteRequest
	ops *koraOps
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
		if !api.ops.hasDocs() {
			return goja.Null()
		}
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kora.getDoc requires 2 arguments: doctype, name"))
		}
		doctype := call.Arguments[0].String()
		name := call.Arguments[1].String()
		doc, err := api.ops.getDoc(doctype, name)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		if doc == nil {
			return goja.Null()
		}
		return vm.ToValue(doc)
	})

	// kora.hasDocType(doctype) → boolean; reveals only whether the record type is installed
	obj.Set("hasDocType", func(call goja.FunctionCall) goja.Value { return api.hasDocType(vm, call) })

	// kora.getList(doctype, {filters, orderBy, limit, offset}) → array
	obj.Set("getList", func(call goja.FunctionCall) goja.Value {
		if !api.ops.hasDocs() {
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
		docs, err := api.ops.getList(doctype, opts)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return vm.ToValue(docs)
	})

	// kora.saveDoc(doctype, doc) → document
	obj.Set("saveDoc", func(call goja.FunctionCall) goja.Value {
		if !api.ops.hasDocs() {
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
		saved, err := api.ops.saveDoc(doctype, doc)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return vm.ToValue(saved)
	})

	// kora.createDoc(doctype, doc) → document
	obj.Set("createDoc", func(call goja.FunctionCall) goja.Value {
		if !api.ops.hasDocs() {
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
		created, err := api.ops.createDoc(doctype, doc)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return vm.ToValue(created)
	})

	// kora.deleteDoc(doctype, name) → void
	obj.Set("deleteDoc", func(call goja.FunctionCall) goja.Value {
		if !api.ops.hasDocs() {
			return goja.Undefined()
		}
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kora.deleteDoc requires 2 arguments: doctype, name"))
		}
		doctype := call.Arguments[0].String()
		name := call.Arguments[1].String()
		if err := api.ops.deleteDoc(doctype, name); err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return goja.Undefined()
	})

	// kora.secrets.get(key) → string
	secretsObj := vm.NewObject()
	secretsObj.Set("get", func(call goja.FunctionCall) goja.Value {
		if !api.ops.hasSecrets() {
			return goja.Null()
		}
		if len(call.Arguments) < 1 {
			panic(vm.ToValue("kora.secrets.get requires 1 argument: key"))
		}
		val := api.ops.getSecret(call.Arguments[0].String())
		if val == nil {
			return goja.Null()
		}
		return vm.ToValue(val)
	})
	obj.Set("secrets", secretsObj)

	obj.Set("http", api.httpObject(vm))

	return obj
}

// httpObject builds the kora.http global: outbound requests through the HTTP provider.
func (api *koraAPI) httpObject(vm *goja.Runtime) goja.Value {
	httpObj := vm.NewObject()
	httpObj.Set("fetch", func(call goja.FunctionCall) goja.Value {
		if !api.ops.hasHTTP() {
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

		resp, err := api.ops.fetch(url, opts)
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}

		return api.httpResponseObject(vm, resp)
	})
	return httpObj
}

// httpResponseObject wraps an outbound response as the object scripts receive from kora.http.fetch.
func (api *koraAPI) httpResponseObject(vm *goja.Runtime, resp *HTTPResponse) goja.Value {
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
}

// hasDocType answers kora.hasDocType(doctype). Without a DocTypeChecker provider it is false.
func (api *koraAPI) hasDocType(vm *goja.Runtime, call goja.FunctionCall) goja.Value {
	if len(call.Arguments) < 1 {
		panic(vm.ToValue("kora.hasDocType requires 1 argument: doctype"))
	}
	return vm.ToValue(api.ops.hasDocType(call.Arguments[0].String()))
}

func (api *koraAPI) log(level string, call goja.FunctionCall) {
	msg := ""
	if len(call.Arguments) > 0 {
		msg = call.Arguments[0].String()
	}
	api.ops.log(level, msg)
}
