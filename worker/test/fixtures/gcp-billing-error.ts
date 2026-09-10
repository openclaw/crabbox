export const gcpBillingMessage =
  "This API method requires billing to be enabled. Please enable billing on project #123456789 then retry";

export const gcpBillingError = {
  error: {
    code: 403,
    message: gcpBillingMessage,
    errors: [{ message: gcpBillingMessage, domain: "global", reason: "forbidden" }],
  },
};

export const gcpBillingBody = JSON.stringify(gcpBillingError, null, 2);
