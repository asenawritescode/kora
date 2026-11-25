// Records a deposit or withdrawal and keeps the member's savings balance. Runs on create
// only: an entry is append-only. A withdrawal above the member's savings is refused.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

if (oldDoc) {
  throw new Error("A savings entry cannot be changed after saving. Record an opposite entry instead.");
}

var member = kora.getDoc("Member", String(doc.member || ""));
if (!member) {
  throw new Error("Cannot check member " + doc.member + ".");
}
if (member.status === "Exited") {
  throw new Error(member.member_name + " has exited; no new savings entries.");
}

var balance = money(member.savings_balance);
var amount = money(doc.amount);
if (doc.entry_type === "Deposit") {
  balance = money(balance + amount);
} else if (doc.entry_type === "Withdrawal") {
  if (amount > balance) {
    throw new Error(member.member_name + " has " + balance + " in savings; cannot withdraw " + amount + ".");
  }
  balance = money(balance - amount);
} else {
  throw new Error("A savings entry is a Deposit or a Withdrawal, not " + doc.entry_type + ".");
}

kora.saveDoc("Member", { name: member.name, savings_balance: balance });
return { doc: doc };
