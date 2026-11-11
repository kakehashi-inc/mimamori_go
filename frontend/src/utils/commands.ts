/**
 * The command that initializes an agent. It asks for the token on the
 * terminal without echo, so the value never appears in the process list, a
 * command argument or the shell history.
 */
export function agentInitCommand(url: string, fingerprint?: string, overwrite?: boolean): string {
    const fp = fingerprint ? ` --ca-fingerprint ${fingerprint}` : '';
    const ow = overwrite ? ' --overwrite' : '';
    return `./mimamori agent init --server ${url}${fp}${ow}`;
}
