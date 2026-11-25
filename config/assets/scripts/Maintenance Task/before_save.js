// A repair or service on an asset. Marking a task Done returns an asset that is Under repair
// to service, unless another planned task on it is already due. Done and Cancelled tasks are final.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function day(value) {
  return value ? String(value).slice(0, 10) : "";
}

function loadAsset(name) {
  var asset = kora.getDoc("Asset", name);
  if (!asset) {
    throw new Error("Cannot check asset " + name + ".");
  }
  return asset;
}

function hasOtherDuePlannedTask(task, today) {
  var tasks = kora.getList("Maintenance Task", { filters: { asset: task.asset, status: "Planned" }, limit: 500 });
  for (var i = 0; i < tasks.length; i++) {
    if (task.name && tasks[i].name === task.name) {
      continue;
    }
    if (day(tasks[i].due_date) <= today) {
      return true;
    }
  }
  return false;
}

var ref = (oldDoc && (oldDoc.reference || oldDoc.name)) || doc.reference || "This task";

if (oldDoc && (oldDoc.status === "Done" || oldDoc.status === "Cancelled") && doc.status !== oldDoc.status) {
  throw new Error(ref + " is " + oldDoc.status + " and its status cannot change.");
}

if (!oldDoc) {
  var newAsset = loadAsset(String(doc.asset || ""));
  if (newAsset.status === "Retired") {
    throw new Error((newAsset.asset_name || newAsset.name) + " is retired and takes no maintenance tasks.");
  }
}

var becomesDone = doc.status === "Done" && (!oldDoc || oldDoc.status !== "Done");
if (becomesDone) {
  var repaired = loadAsset(String(doc.asset || ""));
  if (repaired.status === "Under repair" && !hasOtherDuePlannedTask(doc, new Date().toISOString().slice(0, 10))) {
    kora.saveDoc("Asset", { name: repaired.name, status: repaired.assigned_to ? "In use" : "In store" });
  }
}

return { doc: doc };
