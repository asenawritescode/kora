// Keep direct document writes consistent with the workflow's exception gate.
var doc = __kora_event__.doc;
if ((doc.status === "Past Due" || doc.status === "Paused" || doc.status === "Cancelled") &&
    !String(doc.exception_reason || "").trim()) {
  throw new Error("Explain why service is past due, paused, or cancelled.");
}
return { doc: doc };
