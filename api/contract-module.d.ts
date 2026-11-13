// Types for the part of contracts/lib/contract.mjs that vitest.config.ts uses.
// The module is plain JavaScript with JSDoc types, which this folder's
// TypeScript setup does not read from node_modules.
declare module "@homeport/contracts/lib/contract.mjs" {
  export interface ValidationResult {
    /** Empty when the body is valid. */
    errors: string[];
  }

  export interface Contract {
    validateRequest(method: string, apiPath: string, body: unknown): ValidationResult;
    validateResponse(method: string, apiPath: string, status: number | string, body: unknown): ValidationResult;
  }

  export function loadContract(specPath?: string): Promise<Contract>;
}
