// Leave Request rules for the People pack. A person sets the status; this script refuses
// what the rules forbid, and moves the employee On leave while an approved request covers today.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

var ACTIVE = "Active";
var ON_LEAVE = "On leave";
var LEFT = "Left";

// Status changes allowed from each status. A new request starts Pending or Approved.
var NEXT = {
  "Pending": ["Approved", "Rejected", "Cancelled"],
  "Approved": ["Cancelled"]
};

function ymd(value) {
  return String(value || "").slice(0, 10);
}

// Today as YYYY-MM-DD in UTC: scripts have no tenant time zone.
function today() {
  return new Date().toISOString().slice(0, 10);
}

// Inclusive day count between two YYYY-MM-DD dates.
function daysBetween(first, last) {
  var from = Date.parse(first + "T00:00:00Z");
  var to = Date.parse(last + "T00:00:00Z");
  return Math.round((to - from) / 86400000) + 1;
}

function overlaps(startA, endA, startB, endB) {
  return startA <= endB && startB <= endA;
}

function covers(request, day) {
  return ymd(request.start_date) <= day && day <= ymd(request.end_date);
}

// The employee's requests in the given statuses, not counting this one.
function otherRequests(employee, statuses) {
  var rows = kora.getList("Leave Request", {
    filters: { employee: employee, status: ["in", statuses] },
    limit: 500
  });
  var out = [];
  for (var i = 0; i < rows.length; i++) {
    if (rows[i].name !== doc.name) {
      out.push(rows[i]);
    }
  }
  return out;
}

function requireEmployee(name) {
  var employee = kora.getDoc("Employee", name);
  if (!employee) {
    throw new Error("Cannot check employee " + name + ".");
  }
  return employee;
}

function checkTransition(from, to) {
  if (!oldDoc) {
    if (to !== "Pending" && to !== "Approved") {
      throw new Error("A new leave request is Pending or Approved, not " + to + ".");
    }
    return;
  }
  if (from === to) {
    return;
  }
  var next = NEXT[from] || [];
  if (next.indexOf(to) < 0) {
    throw new Error("A leave request that is " + from.toLowerCase() + " cannot become " + to.toLowerCase() + ".");
  }
}

function hasCap(value) {
  return value !== null && value !== undefined && String(value) !== "";
}

// Approved days of the same leave type that start in the same calendar year, plus this request.
function checkAllowance(employeeName, label, leaveType, start, end) {
  var year = start.slice(0, 4);
  var days = daysBetween(start, end);
  var used = 0;
  var rows = otherRequests(employeeName, ["Approved"]);
  for (var i = 0; i < rows.length; i++) {
    var row = rows[i];
    if (String(row.leave_type || "") === leaveType && ymd(row.start_date).slice(0, 4) === year) {
      used += daysBetween(ymd(row.start_date), ymd(row.end_date));
    }
  }
  var type = kora.getDoc("Leave Type", leaveType);
  if (!type) {
    throw new Error("Cannot check leave type " + leaveType + ".");
  }
  if (!hasCap(type.days_per_year)) {
    return;
  }
  var cap = Number(type.days_per_year);
  var left = cap - used;
  if (days > left) {
    throw new Error(label + " has " + left + " of " + cap + " " + type.leave_type_name + " leave days left in " + year + ".");
  }
}

var status = String(doc.status || "");
var oldStatus = oldDoc ? String(oldDoc.status || "") : "";
var employeeName = String(doc.employee || "");
var employee = requireEmployee(employeeName);
var label = employee.full_name || employeeName;
var start = ymd(doc.start_date);
var end = ymd(doc.end_date);
var becomesApproved = status === "Approved" && oldStatus !== "Approved";
var day = today();

checkTransition(oldStatus, status);

if (!oldDoc && employee.status === LEFT) {
  throw new Error(label + " has left.");
}

if (status === "Pending" || status === "Approved") {
  var open = otherRequests(employeeName, ["Pending", "Approved"]);
  for (var i = 0; i < open.length; i++) {
    if (overlaps(start, end, ymd(open[i].start_date), ymd(open[i].end_date))) {
      throw new Error("These dates overlap leave request " + open[i].name + ".");
    }
  }
}

if (becomesApproved) {
  checkAllowance(employeeName, label, String(doc.leave_type || ""), start, end);
  if (start <= day && day <= end && employee.status === ACTIVE) {
    kora.saveDoc("Employee", { name: employeeName, status: ON_LEAVE });
  }
}

if (oldStatus === "Approved" && status === "Cancelled" && employee.status === ON_LEAVE) {
  var covering = false;
  var approved = otherRequests(employeeName, ["Approved"]);
  for (var j = 0; j < approved.length; j++) {
    if (covers(approved[j], day)) {
      covering = true;
    }
  }
  if (!covering) {
    kora.saveDoc("Employee", { name: employeeName, status: ACTIVE });
  }
}

return { doc: doc };
