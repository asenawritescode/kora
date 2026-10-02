var doc = __kora_event__.doc;
if (doc.batch_status === "recalled" || doc.batch_status === "expired") {
  throw new Error("Recalled or expired medicine batches cannot be dispensed.");
}
