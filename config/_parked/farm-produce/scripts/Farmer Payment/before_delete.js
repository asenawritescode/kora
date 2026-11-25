// A paid payment has moved money on its deliveries; void it instead of deleting it.
var doc = __kora_event__.doc;

if (doc.status === "Paid") {
  throw new Error((doc.reference || doc.name) + " is Paid; void it instead of deleting it.");
}

return { doc: doc };
