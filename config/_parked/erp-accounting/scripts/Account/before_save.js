// A child account has its parent's type. An account used on a posted entry keeps its type.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

// The first posted entry with a line on this account, or "" when none has one.
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

var label = doc.code + " " + doc.account_name;

if (doc.parent_account) {
  var parent = kora.getDoc("Account", String(doc.parent_account));
  if (!parent) {
    throw new Error("Cannot check account " + doc.parent_account + ".");
  }
  if (parent.account_type !== doc.account_type) {
    throw new Error(parent.code + " " + parent.account_name + " is " + parent.account_type + "; a child account must have the same type.");
  }
}

if (oldDoc && oldDoc.account_type !== doc.account_type) {
  var children = kora.getList("Account", { filters: { parent_account: doc.name }, limit: 500 });
  if (children.some(function (child) { return child.account_type !== doc.account_type; })) {
    throw new Error(label + " has child accounts; they must have the same type.");
  }
  var used = postedUse(doc.name);
  if (used) {
    throw new Error(label + " is used on posted entry " + used + "; its type cannot change.");
  }
}

return { doc: doc };
