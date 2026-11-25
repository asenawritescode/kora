// A booking's stay must not overlap another active stay in the same room, and its status
// moves the room: check-in makes the room Occupied, check-out makes it Cleaning. Stays are
// active while Booked or Checked in; back-to-back stays do not overlap.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function blank(value) {
  return value === undefined || value === null || String(value).trim() === "";
}

// Dates come back as "YYYY-MM-DD" text, possibly with a time part; compare the day only.
function day(value) {
  return String(value || "").slice(0, 10);
}

function isActive(status) {
  return status === "Booked" || status === "Checked in";
}

function loadRoom(name) {
  var room = kora.getDoc("Room", name);
  if (!room) {
    throw new Error("Cannot check room " + name + ".");
  }
  return room;
}

function setRoomStatus(room, status) {
  kora.saveDoc("Room", { name: room.name, status: status });
}

// Refuses when another active booking of the same room overlaps this stay.
function checkOverlap(room) {
  var ci = day(doc.check_in);
  var co = day(doc.check_out);
  var others = kora.getList("Booking", {
    filters: {
      room: room.name,
      status: ["in", ["Booked", "Checked in"]],
      check_in: ["<", co],
      check_out: [">", ci]
    },
    limit: 500
  });
  for (var i = 0; i < others.length; i++) {
    var other = others[i];
    if (other.name === doc.name) {
      continue;
    }
    // Re-check the dates here too: the filter narrows the list, this decides.
    if (ci < day(other.check_out) && day(other.check_in) < co) {
      throw new Error("Room " + room.room_no + " is booked " + day(other.check_in) + " to " +
        day(other.check_out) + " (" + (other.reference || other.name) + ").");
    }
  }
}

function checkIn(room) {
  if (room.status !== "Available") {
    throw new Error("Room " + room.room_no + " is " + room.status + ".");
  }
  setRoomStatus(room, "Occupied");
}

function checkOut(room) {
  setRoomStatus(room, "Cleaning");
}

var room = loadRoom(doc.room);
var was = oldDoc ? oldDoc.status : null;
var status = doc.status;

if (!oldDoc) {
  if (status !== "Booked" && status !== "Checked in") {
    throw new Error("A new booking must be Booked or Checked in, not " + status + ".");
  }
  if (blank(doc.rate) && !blank(room.rate_per_night)) {
    doc.rate = room.rate_per_night;
  }
}

if (status !== was) {
  if (status === "Checked in" && (was === null || was === "Booked")) {
    checkIn(room);
  } else if (status === "Checked out" && was === "Checked in") {
    checkOut(room);
  } else if (status === "Cancelled" && was === "Booked") {
    // A cancelled booking holds no room: nothing to change.
  } else if (status === "Cancelled" && was === "Checked in") {
    throw new Error("Check the guest out instead.");
  } else if (oldDoc) {
    throw new Error("A booking cannot change from " + was + " to " + status +
      ". Allowed: Booked to Checked in or Cancelled; Checked in to Checked out.");
  }
}

if (isActive(status)) {
  checkOverlap(room);
}

return { doc: doc };
