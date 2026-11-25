// A task is added to, or moved into, a project only while that project is open.
// Changing the status of a task that stays in its project is always allowed.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

if (!oldDoc || String(doc.project || "") !== String(oldDoc.project || "")) {
  var project = kora.getDoc("Project", String(doc.project || ""));
  if (!project) {
    throw new Error("Cannot check project " + (doc.project || "(blank)") + ".");
  }
  if (project.status === "Done" || project.status === "Cancelled") {
    throw new Error((project.project_name || project.name) + " is " + project.status + "; its tasks cannot be added or moved there.");
  }
}

return { doc: doc };
