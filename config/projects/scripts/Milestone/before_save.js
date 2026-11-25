// A new milestone is not added to a project that is Done or Cancelled.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

if (!oldDoc) {
  var project = kora.getDoc("Project", String(doc.project || ""));
  if (!project) {
    throw new Error("Cannot check project " + (doc.project || "(blank)") + ".");
  }
  if (project.status === "Done" || project.status === "Cancelled") {
    throw new Error("Milestones cannot be added to " + (project.project_name || project.name) + ": it is " + project.status + ".");
  }
}

return { doc: doc };
