// An account used on a posted entry is part of the books and cannot be deleted.
var doc = __kora_event__.doc;

function postedUse(account) {
  for (var offset = 0; ; offset += 500) {
    var page = kora.getList("Journal Entry", { filters: { status: "Posted" }, orderBy: "name asc", limit: 500, offset: offset });
    for (var i = 0; i < page.length; i++) {
      var entry = kora.getDoc("Journal Entry", page[i].name); // a list read leaves out the lines
      if (!entry) {
        throw new Error("Cannot check journal entry " + page[i].name + ".");
      }
      if ((entry.lines || []).some(function (line) { return String(line.account || "") === account; })) {
        return entry.name;
      }
    }
    if (page.length < 500) {
      return "";
    }
  }
}

var used = postedUse(doc.name);
if (used) {
  throw new Error(doc.code + " " + doc.account_name + " is used on posted entry " + used + " and cannot be deleted.");
}

return { doc: doc };
