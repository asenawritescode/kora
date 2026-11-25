// A lease holds one unit from its start date to its end date (an empty end date is open-ended).
// An Active lease occupies its unit. Two Active leases of one unit may not overlap. Ending a lease
// frees its unit unless another Active lease of that unit remains. An Ended lease is history: it
// never changes a unit, and it cannot be reopened.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function day(value) {
  return String(value || "").slice(0, 10);
}

function pad(n) {
  return n < 10 ? "0" + n : String(n);
}

function todayText() {
  var now = new Date();
  return now.getFullYear() + "-" + pad(now.getMonth() + 1) + "-" + pad(now.getDate());
}

function unitOf(name) {
  var unit = kora.getDoc("Unit", name);
  if (!unit) {
    throw new Error("Cannot check unit " + name + ".");
  }
  return unit;
}

// otherActiveLeases returns the Active leases of a unit, leaving out the lease being saved.
function otherActiveLeases(unitName, self) {
  return kora.getList("Lease", {filters: {unit: unitName, status: "Active"}, limit: 500})
    .filter(function (lease) { return lease.name !== self; });
}

// overlaps: each lease starts on or before the other's end; an empty end means no end.
function overlaps(start, end, other) {
  var otherStart = day(other.start_date);
  var otherEnd = day(other.end_date);
  return (!otherEnd || start <= otherEnd) && (!end || otherStart <= end);
}

// releaseUnit marks a unit Vacant when no other Active lease holds it.
function releaseUnit(unitName, self) {
  if (otherActiveLeases(unitName, self).length === 0) {
    kora.saveDoc("Unit", {name: unitName, status: "Vacant"});
  }
}

function checkOverlap(unit, unitName, start, end, self) {
  var others = otherActiveLeases(unitName, self);
  for (var i = 0; i < others.length; i++) {
    if (overlaps(start, end, others[i])) {
      var tenant = kora.getDoc("Tenant", others[i].tenant);
      if (!tenant) {
        throw new Error("Cannot check tenant " + others[i].tenant + ".");
      }
      throw new Error("Unit " + unit.unit_no + " is let to " + tenant.tenant_name + " from " +
        day(others[i].start_date) + " (" + (others[i].reference || others[i].name) + ").");
    }
  }
}

var status = String(doc.status || "");
var was = oldDoc ? String(oldDoc.status || "") : "";
var unitName = String(doc.unit || "");
var unit = unitOf(unitName);
var isNew = !oldDoc;
var unitChanged = !isNew && unitName !== String(oldDoc.unit || "");
var datesChanged = !isNew && (day(doc.start_date) !== day(oldDoc.start_date) || day(doc.end_date) !== day(oldDoc.end_date));

if (!isNew && was === "Ended" && status === "Active") {
  throw new Error("Ended lease " + (doc.reference || doc.name) + " cannot be reopened. Make a new lease.");
}

if (status === "Active" && (isNew || unitChanged || datesChanged)) {
  if ((isNew || unitChanged) && unit.status === "Maintenance") {
    throw new Error("Unit " + unit.unit_no + " is under maintenance and cannot be let.");
  }
  checkOverlap(unit, unitName, day(doc.start_date), day(doc.end_date), doc.name);
}

if (status === "Active" && unit.status === "Vacant") {
  kora.saveDoc("Unit", {name: unitName, status: "Occupied"});
}

if (!isNew && was === "Active") {
  if (status === "Ended") {
    if (!day(doc.end_date)) {
      doc.end_date = todayText();
    }
    releaseUnit(unitName, doc.name);
  }
  if (unitChanged) {
    releaseUnit(String(oldDoc.unit), doc.name);
  }
}

return { doc: doc };
