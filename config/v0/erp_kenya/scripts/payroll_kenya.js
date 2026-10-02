// Kenya Payroll Computation Engine
// Handles PAYE, NSSF, SHIF (NHIF), Affordable Housing Levy

// Kenya PAYE Rates (2024)
var PAYE_BANDS = [
  {max: 24000, rate: 10},
  {max: 32333, rate: 25},
  {max: 500000, rate: 30},
  {max: 800000, rate: 32.5},
  {max: Infinity, rate: 35}
];

var PERSONAL_RELIEF = 2400; // Monthly

function calculatePAYE(taxableIncome) {
  if (taxableIncome <= 0) return 0;
  var tax = 0;
  var previousMax = 0;

  for (var i = 0; i < PAYE_BANDS.length; i++) {
    var band = PAYE_BANDS[i];
    var taxableInBand = Math.min(taxableIncome, band.max) - previousMax;
    if (taxableInBand > 0) {
      tax += (taxableInBand * band.rate) / 100;
    }
    previousMax = band.max;
    if (taxableIncome <= band.max) break;
  }

  // Apply personal relief
  tax = Math.max(0, tax - PERSONAL_RELIEF);
  return Math.round(tax * 100) / 100;
}

function calculateNSSF(pensionableEarnings) {
  // New NSSF rates (2024): 6% of pensionable earnings, matched by employer
  var lowerLimit = 6000;
  var upperLimit = 18000;
  var earnings = Math.min(Math.max(pensionableEarnings, lowerLimit), upperLimit);
  return Math.round(earnings * 0.06 * 100) / 100; // Employee contribution (6%)
}

function calculateSHIF(grossPay) {
  // SHIF (Social Health Insurance Fund): 2.75% of gross pay
  return Math.round(grossPay * 0.0275 * 100) / 100;
}

function calculateHousingLevy(grossPay) {
  // Affordable Housing Levy: 1.5% of gross pay
  return Math.round(grossPay * 0.015 * 100) / 100;
}

// Export computation functions for use in payroll processing
// These are called when processing payroll entries
if (__kora_event__.doctype === "Payroll Period" && __kora_event__.action === "before_save") {
  if (doc.status === "Processing") {
    // This would be called during payroll processing
    // The actual processing happens via a scheduled script or API
    kora.log("Payroll processing triggered for " + doc.period_name, "info");
  }
}
