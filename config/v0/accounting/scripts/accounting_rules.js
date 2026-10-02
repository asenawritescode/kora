var doc = __kora_event__.doc;
if (doc.debit_total !== doc.credit_total) {
  throw new Error("Journal entry debits and credits must balance.");
}
if (doc.status === "Posted" && __kora_event__.action === "edit") {
  throw new Error("Posted journal entries require a reversal or adjustment.");
}
