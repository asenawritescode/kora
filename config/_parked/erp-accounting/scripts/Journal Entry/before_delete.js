// Posted and reversed entries are part of the books; only a Draft can be deleted.
var doc = __kora_event__.doc;

if (doc.status === "Posted" || doc.status === "Reversed") {
  throw new Error((doc.reference || doc.name) + " is " + doc.status + " and cannot be deleted.");
}

return { doc: doc };
