/**
 * The command that initializes an agent. It asks for the token on the
 * terminal without echo, so the value never appears in the process list, a
 * command argument or the shell history.
 */
export function agentInitCommand(
    centralHost: string,
    centralPort: number | string,
    fingerprint?: string,
    overwrite?: boolean
): string {
    const fp = fingerprint ? ` --ca-fingerprint ${fingerprint}` : '';
    const ow = overwrite ? ' --overwrite' : '';
    return `./mimamori agent init --central-host ${centralHost} --central-port ${centralPort}${fp}${ow}`;
}

/** The command that changes the central host of an initialized agent. */
export function agentSetCentralCommand(host: string, port: number | string): string {
    return `./mimamori agent set-central --host ${host} --port ${port}`;
}
