// An entry logs hours against one task, in that task's project, for more than 0 and at
// most 24 hours. New entries are refused on a project that is Done or Cancelled.
// A blank project is refused by the required-field check before this script runs.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function finished(status) {
  return status === "Done" || status === "Cancelled";
}

var hours = Number(doc.hours || 0);
if (!(hours > 0) || hours > 24) {
  throw new Error("Hours must be more than 0 and at most 24.");
}

var task = kora.getDoc("Task", String(doc.task || ""));
if (!task) {
  throw new Error("Cannot check task " + (doc.task || "(blank)") + ".");
}
if (String(doc.project || "") !== String(task.project || "")) {
  throw new Error((task.task_title || task.name) + " belongs to " + task.project + ", not " + (doc.project || "(blank)") + ".");
}

if (!oldDoc) {
  var project = kora.getDoc("Project", String(doc.project || ""));
  if (!project) {
    throw new Error("Cannot check project " + doc.project + ".");
  }
  if (finished(project.status)) {
    throw new Error("Time cannot be logged to " + (project.project_name || project.name) + ": it is " + project.status + ".");
  }
}

return { doc: doc };
