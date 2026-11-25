// A trip On the road is still with its vehicle and driver; finish or cancel it first.
var doc = __kora_event__.doc;

if (doc.status === "On the road") {
  throw new Error("Finish or cancel " + (doc.reference || doc.name) + " first.");
}

return { doc: doc };
