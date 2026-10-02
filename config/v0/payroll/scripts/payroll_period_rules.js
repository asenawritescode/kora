var doc = __kora_event__.doc;
if (doc.status === "locked" && __kora_event__.action === "edit") {
  throw new Error("Locked payroll periods require an approved adjustment.");
}
