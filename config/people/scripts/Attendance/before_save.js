// Attendance rule for the People pack: a day inside an approved leave request is not Present.
// Absent, Late and Half day stay as the person chose them. A person sets On leave instead.
var doc = __kora_event__.doc;

function ymd(value) {
  return String(value || "").slice(0, 10);
}

if (doc.status === "Present") {
  var employeeName = String(doc.employee || "");
  var employee = kora.getDoc("Employee", employeeName);
  if (!employee) {
    throw new Error("Cannot check employee " + employeeName + ".");
  }
  var day = ymd(doc.attendance_date);
  var leave = kora.getList("Leave Request", {
    filters: { employee: employeeName, status: "Approved" },
    limit: 500
  });
  for (var i = 0; i < leave.length; i++) {
    if (ymd(leave[i].start_date) <= day && day <= ymd(leave[i].end_date)) {
      throw new Error((employee.full_name || employeeName) + " has approved leave on " + day + "; set the status to On leave.");
    }
  }
}

return { doc: doc };
