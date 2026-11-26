// Periods cannot overlap. Reopening a Closed period is allowed.
var doc = __kora_event__.doc;

function dateText(value) {
  return value instanceof Date ? value.toISOString().slice(0, 10) : String(value || "").slice(0, 10);
}

var overlapping = kora.getList("Accounting Period", {
  filters: { start_date: ["<=", dateText(doc.end_date)], end_date: [">=", dateText(doc.start_date)] },
  limit: 500
});
for (var i = 0; i < overlapping.length; i++) {
  if (overlapping[i].name !== doc.name) {
    throw new Error(doc.period_name + " overlaps " + overlapping[i].period_name + ".");
  }
}

return { doc: doc };
