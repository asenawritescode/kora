// An Active lease still holds its unit: end it before it is deleted. An Ended lease is history and
// can be deleted; deleting it changes no unit.
var doc = __kora_event__.doc;

if (String(doc.status || "") === "Active") {
  throw new Error("End " + (doc.reference || doc.name) + " first.");
}

return { doc: doc };
