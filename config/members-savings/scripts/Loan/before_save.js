// Guards a loan's status and terms. A new loan starts as Applied, Approved or Rejected.
// A person moves it Applied -> Approved or Rejected, Approved -> Rejected or Disbursed.
// Repaid is set only by a repayment: the repayment script's own save changes amount_repaid
// in the same save as the status.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

var allowedMoves = {
  Applied: ["Approved", "Rejected"],
  Approved: ["Rejected", "Disbursed"]
};

function movesFrom(status) {
  var moves = allowedMoves[status] || [];
  return moves.length === 0 ? "none" : moves.join(", ");
}

function checkNewLoan() {
  var member = kora.getDoc("Member", String(doc.member || ""));
  if (!member) {
    throw new Error("Cannot check member " + doc.member + ".");
  }
  if (member.status === "Exited") {
    throw new Error(member.member_name + " has exited; no new loans.");
  }
  if (doc.status !== "Applied" && doc.status !== "Approved" && doc.status !== "Rejected") {
    throw new Error("A new loan starts as Applied, Approved or Rejected, not " + doc.status + ".");
  }
}

function checkDisbursedTerms() {
  if (oldDoc.status !== "Disbursed") {
    return;
  }
  if (String(doc.member || "") !== String(oldDoc.member || "") ||
      money(doc.amount) !== money(oldDoc.amount) ||
      money(doc.interest_rate) !== money(oldDoc.interest_rate) ||
      Number(doc.term_months || 0) !== Number(oldDoc.term_months || 0)) {
    throw new Error("A disbursed loan's member, amount, interest rate and term cannot change.");
  }
}

function checkRepaid() {
  if (oldDoc.status !== "Disbursed" || money(doc.amount_repaid) === money(oldDoc.amount_repaid)) {
    throw new Error("A loan becomes Repaid when repayments cover it.");
  }
  var owed = money(money(doc.amount) - money(doc.amount_repaid));
  if (owed > 0) {
    throw new Error("The loan still owes " + owed + "; it becomes Repaid only when repayments cover it.");
  }
}

function checkStatusMove() {
  if (doc.status === oldDoc.status) {
    return;
  }
  if (doc.status === "Repaid") {
    checkRepaid();
    return;
  }
  var moves = allowedMoves[oldDoc.status] || [];
  if (moves.indexOf(doc.status) < 0) {
    throw new Error("A loan cannot change from " + oldDoc.status + " to " + doc.status +
      ". Allowed moves from " + oldDoc.status + ": " + movesFrom(oldDoc.status) + ".");
  }
}

if (!oldDoc) {
  checkNewLoan();
} else {
  checkDisbursedTerms();
  checkStatusMove();
}
return { doc: doc };
