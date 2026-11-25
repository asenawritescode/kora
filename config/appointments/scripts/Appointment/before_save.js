// Keeps bookings sound. A new or moved appointment must use an offered service; a
// booked stylist may not take two overlapping bookings; a Done, No show or Cancelled
// appointment stays that way. Refusing here rolls back the whole save.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

var FINAL_STATUSES = ["Done", "No show", "Cancelled"];
var MINUTE_MS = 60000;
var DAY_MINUTES = 1440;
var services = {};

function minutesOf(service) {
  var length = Math.round(Number(service.duration_minutes || 0));
  return length > 0 ? length : 30;
}

function offered(value) {
  return value === true || value === 1 || String(value) === "1" || String(value) === "true";
}

// Minutes since the epoch. Naive values are read as UTC, so every value of one
// dialect compares on the same clock. Accepts "2026-12-01 10:00:00",
// "2026-12-01T10:00", and a trailing Z or +hh:mm / +hhmm offset.
function minuteOf(value) {
  var text = String(value || "");
  var m = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2}))?(?:\.\d+)?(Z|[+-]\d{2}:?\d{2})?$/.exec(text);
  if (!m) {
    throw new Error("Cannot read the time " + text + ".");
  }
  var ms = Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]), Number(m[4]), Number(m[5]), Number(m[6] || 0));
  var zone = m[7];
  if (zone && zone !== "Z") {
    var sign = zone.charAt(0) === "-" ? -1 : 1;
    var digits = zone.slice(1).replace(":", "");
    ms -= sign * (Number(digits.slice(0, 2)) * 60 + Number(digits.slice(2, 4))) * MINUTE_MS;
  }
  return Math.floor(ms / MINUTE_MS);
}

// The wall-clock time as shown to a person, e.g. "10:30".
function clockOf(value) {
  var m = /[T ](\d{2}:\d{2})/.exec(String(value || ""));
  return m ? m[1] : String(value || "");
}

function serviceOf(name) {
  if (!name) {
    throw new Error("An appointment needs a service.");
  }
  if (!services.hasOwnProperty(name)) {
    var service = kora.getDoc("Service", name);
    if (!service) {
      throw new Error("Cannot check service " + name + ".");
    }
    services[name] = service;
  }
  return services[name];
}

// Fails the save when the appointment's service is no longer offered. Checked when the
// appointment is new or its service changes, so an old booking can still be edited.
function checkOffered() {
  if (oldDoc && String(doc.service || "") === String(oldDoc.service || "")) {
    return;
  }
  var service = serviceOf(doc.service);
  if (!offered(service.active)) {
    throw new Error(service.service_name + " is not offered now.");
  }
}

// Fails when the stylist already has a Booked appointment that overlaps this one.
// An appointment occupies [start, start + duration). Back-to-back is allowed.
function checkNoOverlap() {
  if (!doc.staff || doc.status !== "Booked") {
    return;
  }
  if (!doc.scheduled_for) {
    throw new Error("An appointment needs a time.");
  }
  var start = minuteOf(doc.scheduled_for);
  var end = start + minutesOf(serviceOf(doc.service));
  // Start from the day before, so an appointment that began late yesterday is seen.
  var fromDay = (Math.floor(start / DAY_MINUTES) - 1) * DAY_MINUTES;
  var from = new Date(fromDay * MINUTE_MS).toISOString().slice(0, 10);
  var others = kora.getList("Appointment", {
    filters: { staff: String(doc.staff), status: "Booked", scheduled_for: [">=", from] },
    orderBy: "scheduled_for asc",
    limit: 500
  });
  if (others.length >= 500) {
    throw new Error("Cannot check " + doc.staff + "'s bookings: there are too many to compare.");
  }
  for (var i = 0; i < others.length; i++) {
    var other = others[i];
    if (other.name === doc.name || other.status !== "Booked" || String(other.staff || "") !== String(doc.staff)) {
      continue;
    }
    var otherStart = minuteOf(other.scheduled_for);
    var otherEnd = otherStart + minutesOf(serviceOf(other.service));
    if (start < otherEnd && otherStart < end) {
      throw new Error(doc.staff + " has " + other.client_name + " at " + clockOf(other.scheduled_for) +
        " (" + (other.reference || other.name) + ").");
    }
  }
}

// A new appointment starts Booked. Done, No show and Cancelled are final.
function checkStatus() {
  if (!oldDoc) {
    if (!doc.status) {
      doc.status = "Booked";
    }
    if (doc.status !== "Booked") {
      throw new Error("A new appointment starts as Booked.");
    }
    return;
  }
  if (FINAL_STATUSES.indexOf(oldDoc.status) >= 0 && doc.status !== oldDoc.status) {
    throw new Error((oldDoc.reference || oldDoc.name) + " is " + oldDoc.status + "; it cannot change to " + doc.status + ".");
  }
}

checkStatus();
checkOffered();
checkNoOverlap();

return { doc: doc };
