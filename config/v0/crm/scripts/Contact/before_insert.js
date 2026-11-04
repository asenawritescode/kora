var doc = __kora_event__.doc;
var first = String(doc.first_name || "").trim();
var last = String(doc.last_name || "").trim();
doc.full_name = [first, last].filter(Boolean).join(" ");
return { doc: doc };
