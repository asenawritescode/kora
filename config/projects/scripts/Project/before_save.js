// A project is finished only when its work is. Setting it to Done or Cancelled is
// refused while any task of the project is not Done; tasks are not closed for you.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function finished(status) {
  return status === "Done" || status === "Cancelled";
}

// A new project has no tasks yet, so only a status change to a finished state is checked.
if (oldDoc && finished(doc.status) && doc.status !== oldDoc.status) {
  var open = kora.getList("Task", {
    filters: { project: doc.name, status: ["!=", "Done"] },
    orderBy: "task_title asc",
    limit: 500
  });
  if (open.length > 0) {
    var titles = [];
    for (var i = 0; i < open.length && i < 3; i++) {
      titles.push(open[i].task_title || open[i].name);
    }
    var label = doc.project_name || doc.name;
    throw new Error(label + " has " + open.length + " open " + (open.length === 1 ? "task" : "tasks") + ": " + titles.join(", ") + ".");
  }
}

return { doc: doc };
