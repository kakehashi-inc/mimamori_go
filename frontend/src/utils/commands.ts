/**
 * The command that initializes an agent. The token is passed through stdin
 * so it does not appear in the process list or as a command argument.
 */
export function agentInitCommand(value: string, url: string, fingerprint?: string, overwrite?: boolean): string {
    const fp = fingerprint ? ` --ca-fingerprint ${fingerprint}` : '';
    const ow = overwrite ? ' --overwrite' : '';
    return `printf '%s\\n' '${value}' | sudo mimamori agent init --server ${url}${fp}${ow}`;
}
