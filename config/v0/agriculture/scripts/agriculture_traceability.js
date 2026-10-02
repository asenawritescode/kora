var doc = __kora_event__.doc;
if (doc.status === "harvested" && (!doc.field || !doc.season || !doc.batch_reference)) {
  throw new Error("Harvest closure requires field, season, and batch evidence.");
}
