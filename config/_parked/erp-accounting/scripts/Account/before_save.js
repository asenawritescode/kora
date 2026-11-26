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

// The account type each posting role needs; postings rely on it.
var ROLE_TYPES = {
  "Cash": "Asset", "Mobile Money": "Asset", "Bank": "Asset", "Receivable": "Asset", "Inventory": "Asset",
  "Supplier Advances": "Asset", "Payable": "Liability", "Goods Received Not Billed": "Liability",
  "Tax Payable": "Liability", "Cash Clearing": "Asset", "Sales": "Income", "Cost of Sales": "Expense",
  "Stock Adjustments": "Expense", "Expenses": "Expense"
};

// An active account with a posting role is the only active one with that role, has the
// role's type and takes postings.
function checkPostingRole() {
  var role = String(doc.posting_role || "");
  if (!role || doc.status === "Inactive") {
    return;
  }
  if (ROLE_TYPES[role] && ROLE_TYPES[role] !== doc.account_type) {
    throw new Error("An account used for " + role + " must be " + ROLE_TYPES[role] + ", not " + doc.account_type + ".");
  }
  if (String(doc.allow_posting) === "0" || doc.allow_posting === false) {
    throw new Error("An account used for " + role + " must take postings.");
  }
  var same = kora.getList("Account", { filters: { posting_role: role, status: "Active" }, limit: 500 });
  for (var i = 0; i < same.length; i++) {
    if (same[i].name !== doc.name) {
      throw new Error(same[i].code + " " + same[i].account_name + " is already used for " + role + ".");
    }
  }
}

checkPostingRole();

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
