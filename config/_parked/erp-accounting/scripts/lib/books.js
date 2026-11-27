// ---- Posting to the books (#815). The one shared library the selling, stock and ----
// purchasing posting scripts call (installed as lib_books by the erp-books pack).
// Active only when the books (erp-books) are installed. Amounts are in the base currency.
// A posting finds its accounts by posting role; a role with no active account gets the
// standard one created (the accountant can rename and renumber it later).
var POSTING_ACCOUNTS = {
  "Cash": ["1000", "Cash", "Asset"],
  "Mobile Money": ["1010", "Mobile Money", "Asset"],
  "Bank": ["1020", "Bank", "Asset"],
  "Cash Clearing": ["1090", "Cash Clearing", "Asset"],
  "Receivable": ["1100", "Accounts Receivable", "Asset"],
  "Inventory": ["1200", "Inventory", "Asset"],
  "Supplier Advances": ["1300", "Supplier Advances", "Asset"],
  "Payable": ["2000", "Accounts Payable", "Liability"],
  "Goods Received Not Billed": ["2100", "Goods Received Not Billed", "Liability"],
  "Tax Payable": ["2200", "Tax Payable", "Liability"],
  "Sales": ["4000", "Sales", "Income"],
  "Cost of Sales": ["5000", "Cost of Sales", "Expense"],
  "Stock Adjustments": ["5100", "Stock Adjustments", "Expense"],
  "Expenses": ["6000", "Expenses", "Expense"]
};

function booksInstalled() {
  return kora.hasDocType("Journal Entry") && kora.hasDocType("Account");
}

function postingMoney(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

function postingDay(value) {
  return String(value || "").slice(0, 10);
}

// postingAccount returns the active account used for role, creating the standard one when
// none exists. A taken standard code moves to the next free number.
function postingAccount(role) {
  var found = kora.getList("Account", { filters: { posting_role: role, status: "Active" }, limit: 2 });
  if (found.length > 0) {
    return found[0].name;
  }
  var standard = POSTING_ACCOUNTS[role];
  if (!standard) {
    throw new Error("No standard account for posting role " + role + ".");
  }
  var code = Number(standard[0]);
  while (kora.getList("Account", { filters: { code: String(code) }, limit: 1 }).length > 0) {
    code++;
  }
  var created = kora.createDoc("Account", {
    code: String(code), account_name: standard[1], account_type: standard[2],
    posting_role: role, allow_posting: 1, status: "Active"
  });
  return created.name;
}

// postingPeriod makes sure a period covers day when the books keep periods: when periods
// exist and none covers day, the calendar month is opened. A Closed period is left for the
// journal rules to refuse.
function postingPeriod(day) {
  if (kora.getList("Accounting Period", { limit: 1 }).length === 0) {
    return;
  }
  var covering = kora.getList("Accounting Period", {
    filters: { start_date: ["<=", day], end_date: [">=", day] }, limit: 1
  });
  if (covering.length > 0) {
    return;
  }
  var year = Number(day.slice(0, 4));
  var month = Number(day.slice(5, 7));
  var last = new Date(Date.UTC(year, month, 0)).getUTCDate();
  kora.createDoc("Accounting Period", {
    period_name: day.slice(0, 7), start_date: day.slice(0, 8) + "01",
    end_date: day.slice(0, 8) + (last < 10 ? "0" + last : String(last)), status: "Open"
  });
}

// postingEntryFor returns the posted entry already written for source and memo, or null.
function postingEntryFor(sourceType, sourceName, memo) {
  var found = kora.getList("Journal Entry", {
    filters: { source_type: sourceType, source_reference: sourceName, status: "Posted" }, limit: 50
  });
  for (var i = 0; i < found.length; i++) {
    if (found[i].memo === memo) {
      return found[i];
    }
  }
  return null;
}

// postEntry writes one posted, balanced entry for a source event, once. lines is a list of
// {role, debit, credit} in the base currency; lines on the same role are netted. An event
// that moves no money writes nothing.
function postEntry(sourceType, sourceName, day, memo, lines) {
  if (!booksInstalled() || postingEntryFor(sourceType, sourceName, memo)) {
    return;
  }
  var net = {};
  var order = [];
  for (var i = 0; i < lines.length; i++) {
    var role = lines[i].role;
    if (!(role in net)) {
      net[role] = 0;
      order.push(role);
    }
    net[role] = postingMoney(net[role] + postingMoney(lines[i].debit) - postingMoney(lines[i].credit));
  }
  var journal = [];
  var debit = 0;
  var credit = 0;
  for (var j = 0; j < order.length; j++) {
    var amount = net[order[j]];
    if (amount === 0) {
      continue;
    }
    var account = postingAccount(order[j]);
    if (amount > 0) {
      journal.push({ account: account, debit: amount, credit: 0 });
      debit = postingMoney(debit + amount);
    } else {
      journal.push({ account: account, debit: 0, credit: -amount });
      credit = postingMoney(credit - amount);
    }
  }
  if (journal.length === 0) {
    return;
  }
  if (debit !== credit) {
    throw new Error("Posting " + sourceType + " " + sourceName + " does not balance (" + debit + " / " + credit + ").");
  }
  postingPeriod(postingDay(day));
  kora.createDoc("Journal Entry", {
    posting_date: postingDay(day), status: "Posted", memo: memo,
    source_type: sourceType, source_reference: sourceName, lines: journal
  });
}

// reverseEntry reverses the entry a source event posted (the journal rules write the
// opposite entry, dated today). Nothing happens when the event never posted.
function reverseEntry(sourceType, sourceName, memo) {
  if (!booksInstalled()) {
    return;
  }
  var entry = postingEntryFor(sourceType, sourceName, memo);
  if (entry) {
    kora.saveDoc("Journal Entry", { name: entry.name, status: "Reversed" });
  }
}
// ---- end of posting block ----
