var doc = __kora_event__.doc;
if (doc.payment_status === "paid" && doc.fulfillment_status === "unallocated") {
  doc.exception_reason = "Payment received while fulfillment remains unallocated.";
}
if (doc.external_event_id && __kora_event__.previous_event_id === doc.external_event_id) {
  return;
}
