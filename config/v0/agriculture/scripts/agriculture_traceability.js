var doc = __kora_event__.doc;
if (doc.status === "harvested" && (!doc.field || !doc.season || !doc.harvest_batch)) {
  throw new Error("Record a field, season, and linked harvest batch before marking this crop plan harvested.");
}
