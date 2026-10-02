var doc = __kora_event__.doc;
if (doc.billing_status === "past_due" && doc.entitlement_status === "active") {
  doc.exception_reason = "Past-due subscription requires configured grace or suspension policy.";
}
