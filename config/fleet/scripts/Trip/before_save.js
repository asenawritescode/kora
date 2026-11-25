// A trip holds its vehicle while it is On the road. Starting it checks that the vehicle
// and driver are free and moves the vehicle to On trip; delivering or cancelling it
// releases the vehicle. The Dispatcher may only read vehicles, so the vehicle save runs
// with this trip's permission.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

// The statuses a trip may move to from each status. Delivered and Cancelled are final.
var moves = {
  "Planned": ["On the road", "Cancelled"],
  "On the road": ["Delivered", "Cancelled"]
};

function day(value) {
  return String(value || "").slice(0, 10);
}

function tripRef(trip) {
  return trip.reference || trip.name || "This trip";
}

function vehicleOf(name) {
  var vehicle = kora.getDoc("Vehicle", name);
  if (!vehicle) {
    throw new Error("Cannot check vehicle " + name + ".");
  }
  return vehicle;
}

function driverOf(name) {
  var driver = kora.getDoc("Driver", name);
  if (!driver) {
    throw new Error("Cannot check driver " + name + ".");
  }
  return driver;
}

// busyOn returns another trip that is On the road with this vehicle or driver, or null.
function busyOn(field, name) {
  var filters = {status: "On the road"};
  filters[field] = name;
  var trips = kora.getList("Trip", {filters: filters, limit: 500});
  for (var i = 0; i < trips.length; i++) {
    if (trips[i].name !== doc.name) {
      return trips[i];
    }
  }
  return null;
}

function startTrip(vehicle, driver) {
  var reg = vehicle.registration || doc.vehicle;
  var who = driver.driver_name || doc.driver;
  if (vehicle.status !== "Available") {
    throw new Error(reg + " is " + vehicle.status + ".");
  }
  if (driver.status !== "Active") {
    throw new Error(who + " is " + driver.status + "; only an Active driver can start a trip.");
  }
  var other = busyOn("vehicle", doc.vehicle);
  if (other) {
    throw new Error(reg + " is on trip " + tripRef(other) + ".");
  }
  other = busyOn("driver", doc.driver);
  if (other) {
    throw new Error(who + " is on trip " + tripRef(other) + ".");
  }
  var expiry = day(driver.licence_expiry);
  if (expiry && expiry < day(doc.start_date)) {
    throw new Error(who + "'s licence expired on " + expiry + ".");
  }
  kora.saveDoc("Vehicle", {name: doc.vehicle, status: "On trip"});
}

var oldStatus = oldDoc ? oldDoc.status : "";
var newStatus = doc.status;
var starting = newStatus === "On the road" && oldStatus !== "On the road";
var finishing = oldStatus === "On the road" && (newStatus === "Delivered" || newStatus === "Cancelled");
var vehicleChanged = !oldDoc || String(doc.vehicle || "") !== String(oldDoc.vehicle || "");
var driverChanged = !oldDoc || String(doc.driver || "") !== String(oldDoc.driver || "");

if (!oldDoc && newStatus !== "Planned" && newStatus !== "On the road") {
  throw new Error("A new trip is Planned or On the road.");
}
if (oldDoc && oldStatus !== newStatus) {
  var allowed = moves[oldStatus] || [];
  if (allowed.indexOf(newStatus) < 0) {
    if (oldStatus === "Planned" && newStatus === "Delivered") {
      throw new Error("Start the trip first.");
    }
    throw new Error(tripRef(doc) + " is " + oldStatus + "; it cannot become " + newStatus + ".");
  }
}

var vehicle = null;
var driver = null;
if (vehicleChanged) {
  vehicle = vehicleOf(doc.vehicle);
  if (vehicle.status === "Retired") {
    throw new Error((vehicle.registration || doc.vehicle) + " is retired; it cannot take a trip.");
  }
}
if (driverChanged) {
  driver = driverOf(doc.driver);
  if (driver.status === "Left") {
    throw new Error((driver.driver_name || doc.driver) + " has left; they cannot take a trip.");
  }
}
if (starting) {
  startTrip(vehicle || vehicleOf(doc.vehicle), driver || driverOf(doc.driver));
}
if (finishing) {
  vehicle = vehicle || vehicleOf(doc.vehicle);
  if (vehicle.status === "On trip") {
    kora.saveDoc("Vehicle", {name: doc.vehicle, status: "Available"});
  }
}

return { doc: doc };
