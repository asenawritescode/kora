# Kigae small-business MVP

This is the base configuration for the small-business POS pilot, organized as
two focused modules: Point of Sale (products, categories, sales) and Finance
(expenses). The Engine provides generic record and transaction operations;
tenant-specific business rules belong in tenant-owned domain capabilities.

Pricing remains a Cloud billing decision, not a field in this application
configuration. The initial commercial test is one business subscription with
monthly or annual billing; validate the KES price with pilot customers before
publishing rates.
