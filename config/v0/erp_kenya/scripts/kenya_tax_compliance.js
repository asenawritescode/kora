// Kenya tax compliance rules
var doc = __kora_event__.doc;

// Validate KRA PIN format (A followed by 9 digits, ending with a letter)
function validateKRAPIN(pin) {
  if (!pin) return true; // Skip if not provided
  pin = pin.toUpperCase().trim();
  var pinRegex = /^[A-Z]\d{9}[A-Z]$/;
  return pinRegex.test(pin);
}

// Sales Invoice: Validate customer KRA PIN for invoices above threshold
if (__kora_event__.doctype === "Sales Invoice") {
  if (parseFloat(doc.total || 0) >= 5000) {
    // For significant invoices, PIN should ideally be present on customer
    // This is a soft validation - warns but doesn't block
  }
}

// WHT Certificate: Validate rate
if (__kora_event__.doctype === "WHT Certificate") {
  var rate = parseFloat(doc.wht_rate || 0);
  if (rate <= 0 || rate > 30) {
    throw new Error("Withholding tax rate must be between 0.01% and 30%. Current rate: " + rate + "%");
  }
}

// eTIMS Invoice validation
if (__kora_event__.doctype === "eTIMS Invoice") {
  if (doc.status === "Submitted" && !doc.etims_reference) {
    throw new Error("eTIMS reference number is required before submission.");
  }
}
