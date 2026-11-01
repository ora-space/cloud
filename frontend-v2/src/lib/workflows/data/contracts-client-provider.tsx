import type { ReactNode } from "react";
import {
  ContractsClientContext,
  type ContractsClient,
} from "./contracts-client";

/** Supplies the client every copied workflow query and mutation hook calls. */
export function ContractsClientProvider({
  client,
  children,
}: {
  client: ContractsClient;
  children: ReactNode;
}) {
  return (
    <ContractsClientContext.Provider value={client}>
      {children}
    </ContractsClientContext.Provider>
  );
}
