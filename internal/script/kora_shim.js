(function (host, setup) {
	var parse = JSON.parse, stringify = JSON.stringify, str = String, objectToString = Object.prototype.toString;

	function call(op, args) {
		var r = parse(host(stringify({op: op, args: args})));
		if ("throw" in r) throw r.throw;
		return r.ok;
	}
	// isPlainObject accepts what goja exports as a map: class instances pass, while Date,
	// arrays, null and the other built-in classes do not.
	function isPlainObject(v) {
		return typeof v === "object" && v !== null && objectToString.call(v) === "[object Object]";
	}
	function response(r) {
		var body = r.body;
		return {
			status: r.status,
			statusText: r.statusText,
			ok: r.status >= 200 && r.status < 300,
			body: body,
			json: function () {
				try { return parse(body); } catch (e) { throw "JSON parse error: " + e; }
			}
		};
	}

	var log = {};
	["debug", "info", "warn", "error"].forEach(function (level) {
		log[level] = function () {
			call("log", [level, arguments.length ? str(arguments[0]) : ""]);
		};
	});

	function getDoc(doctype, name) {
		if (!setup.docs) return null;
		if (arguments.length < 2) throw "kora.getDoc requires 2 arguments: doctype, name";
		return call("getDoc", [str(doctype), str(name)]);
	}
	function getList(doctype, opts) {
		if (!setup.docs) return [];
		if (arguments.length < 1) throw "kora.getList requires at least 1 argument: doctype";
		return call("getList", [str(doctype), isPlainObject(opts) ? opts : {}]);
	}
	function hasDocType(doctype) {
		if (arguments.length < 1) throw "kora.hasDocType requires 1 argument: doctype";
		return call("hasDocType", [str(doctype)]);
	}
	function saveDoc(doctype, doc) {
		if (!setup.docs) return null;
		if (arguments.length < 2) throw "kora.saveDoc requires 2 arguments: doctype, doc";
		var dt = str(doctype);
		if (!isPlainObject(doc)) throw "kora.saveDoc: second argument must be an object";
		return call("saveDoc", [dt, doc]);
	}
	function createDoc(doctype, doc) {
		if (!setup.docs) return null;
		if (arguments.length < 2) throw "kora.createDoc requires 2 arguments: doctype, doc";
		var dt = str(doctype);
		if (!isPlainObject(doc)) throw "kora.createDoc: second argument must be an object";
		return call("createDoc", [dt, doc]);
	}
	function deleteDoc(doctype, name) {
		if (!setup.docs) return;
		if (arguments.length < 2) throw "kora.deleteDoc requires 2 arguments: doctype, name";
		call("deleteDoc", [str(doctype), str(name)]);
	}

	var secrets = {
		get: function (key) {
			if (!setup.secrets) return null;
			if (arguments.length < 1) throw "kora.secrets.get requires 1 argument: key";
			return call("secret", [str(key)]);
		}
	};

	var http = {
		fetch: function (url, opts) {
			if (!setup.http) throw "kora.http.fetch: provider not available";
			if (arguments.length < 2) throw "kora.http.fetch requires 2 arguments: url, options";
			var u = str(url);
			if (!isPlainObject(opts)) throw "kora.http.fetch: second argument must be an options object";
			return response(call("fetch", [u, opts]));
		}
	};

	return {
		log: log,
		context: setup.context,
		now: function () { return globalThis.Date; },
		getDoc: getDoc,
		hasDocType: hasDocType,
		getList: getList,
		saveDoc: saveDoc,
		createDoc: createDoc,
		deleteDoc: deleteDoc,
		secrets: secrets,
		http: http
	};
})