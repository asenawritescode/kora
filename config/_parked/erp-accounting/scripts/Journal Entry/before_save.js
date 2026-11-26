// Entries: a Draft is free to change while its lines balance and name postable accounts.
// Posting checks the accounting period. A posted or reversed entry is frozen (memo may
// change). Posted to Reversed writes the opposite entry, dated today, in the same save.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

function dateText(value) {
  return value instanceof Date ? value.toISOString().slice(0, 10) : String(value || "").slice(0, 10);
}

function isTrue(value) {
  return value === true || String(value) === "1" || String(value) === "true";
}

function frozenChanged(doc, oldDoc) {
  var same = dateText(doc.posting_date) === dateText(oldDoc.posting_date) &&
    String(doc.period || "") === String(oldDoc.period || "") &&
    String(doc.currency || "") === String(oldDoc.currency || "") &&
    Number(doc.exchange_rate || 0) === Number(oldDoc.exchange_rate || 0);
  var before = oldDoc.lines || [];
  var after = doc.lines || [];
  if (!same || before.length !== after.length) {
    return true;
  }
  for (var i = 0; i < before.length; i++) {
    if (String(before[i].account || "") !== String(after[i].account || "") ||
        String(before[i].description || "") !== String(after[i].description || "") ||
        money(before[i].debit) !== money(after[i].debit) ||
        money(before[i].credit) !== money(after[i].credit)) {
      return true;
    }
  }
  return false;
}

function checkLines(doc) {
  var lines = doc.lines || [];
  if (lines.length < 2) {
    throw new Error("A journal entry needs at least two lines.");
  }
  for (var i = 0; i < lines.length; i++) {
    var n = i + 1;
    var debit = money(lines[i].debit);
    var credit = money(lines[i].credit);
    if (!lines[i].account) {
      throw new Error("Line " + n + " needs an account.");
    }
    if (debit < 0 || credit < 0) {
      throw new Error("Line " + n + " cannot be negative.");
    }
    if (debit > 0 && credit > 0) {
      throw new Error("Line " + n + " cannot have a debit and a credit.");
    }
    if (debit === 0 && credit === 0) {
      throw new Error("Line " + n + " needs a debit or a credit.");
    }
    var account = kora.getDoc("Account", String(lines[i].account));
    if (!account) {
      throw new Error("Cannot check account " + lines[i].account + ".");
    }
    var label = account.code + " " + account.account_name;
    if (account.status !== "Active") {
      throw new Error(label + " is inactive.");
    }
    if (!isTrue(account.allow_posting)) {
      throw new Error(label + " is a group account and takes no postings.");
    }
  }
}

// A blank period is filled with the one covering the date. A posting needs an Open
// period covering its date once any period exists.
function checkPeriod(doc, posting) {
  var date = dateText(doc.posting_date);
  if (!doc.period) {
    var found = kora.getList("Accounting Period", {
      filters: { start_date: ["<=", date], end_date: [">=", date] },
      limit: 1
    });
    if (found.length > 0) {
      doc.period = found[0].name;
    } else if (posting && kora.getList("Accounting Period", { filters: {}, limit: 1 }).length > 0) {
      throw new Error("No accounting period covers " + date + ".");
    }
  }
  if (!doc.period) {
    return;
  }
  var period = kora.getDoc("Accounting Period", String(doc.period));
  if (!period) {
    throw new Error("Cannot check period " + doc.period + ".");
  }
  if (dateText(period.start_date) > date || dateText(period.end_date) < date) {
    throw new Error(period.period_name + " does not cover " + date + ".");
  }
  if (posting && period.status !== "Open") {
    throw new Error(period.period_name + " is " + period.status + ".");
  }
}

// The opposite entry: same accounts, debit and credit swapped, dated today. The engine
// skips this script for the entry written here, so its checks run now.
function writeReversal(doc) {
  var lines = (doc.lines || []).map(function (line) {
    return { account: String(line.account), description: line.description || "", debit: money(line.credit), credit: money(line.debit) };
  });
  var reversal = {
    posting_date: new Date().toISOString().slice(0, 10),
    exchange_rate: Number(doc.exchange_rate || 1),
    status: "Posted",
    memo: "Reversal of " + (doc.reference || doc.name),
    source_type: "Journal Entry",
    source_reference: doc.name,
    lines: lines
  };
  if (doc.currency) {
    reversal.currency = String(doc.currency);
  }
  checkLines(reversal);
  checkPeriod(reversal, true);
  kora.createDoc("Journal Entry", reversal);
}

var from = oldDoc ? oldDoc.status : null;
var to = doc.status;

if (from === "Posted" || from === "Reversed") {
  if (from === "Reversed" && to !== "Reversed") {
    throw new Error("A reversed entry is final.");
  }
  if (to === "Draft") {
    throw new Error("A posted entry cannot go back to Draft.");
  }
  if (frozenChanged(doc, oldDoc)) {
    throw new Error("A posted or reversed entry's date, period, currency, exchange rate and lines cannot change.");
  }
  if (from === "Posted" && to === "Reversed") {
    writeReversal(doc);
  }
  return { doc: doc };
}

if (to === "Reversed") {
  throw new Error("An entry is reversed only after it is posted.");
}
checkLines(doc);
checkPeriod(doc, to === "Posted");

return { doc: doc };
