import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import {
    createAccount,
    deleteAccount,
    listAccounts,
    resetAccountPassword,
    setAccountDisabled,
    setAccountDisplayName,
    setAccountEmail,
    setAccountRole,
} from '../api/client';
import { Badge } from '../components/Badge';
import { LANG_SETTINGS, MAX_DISPLAY_NAME_LENGTH, MIN_PASSWORD_LENGTH, type LangSetting } from '../constants';
import { Button } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { emailInputProps, Field, inputClass } from '../components/FormControls';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { RoleBadge } from '../components/StatusBadges';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { Account, Role } from '../types';
import { accountErrorMessage } from '../utils/accountErrors';
import { formatDateTime } from '../utils/format';

/** How an account is named in dialogs: display name and login ID. */
function accountLabel(a: Account): string {
    return `${a.display_name} (${a.email})`;
}

type Dialog =
    | { kind: 'create' }
    | { kind: 'email'; account: Account }
    | { kind: 'displayName'; account: Account }
    | { kind: 'role'; account: Account }
    | { kind: 'password'; account: Account }
    | { kind: 'toggle'; account: Account }
    | { kind: 'delete'; account: Account }
    | null;

export function AccountsPage() {
    const { t } = useTranslation();
    const { me, refreshMe } = useAuth();
    const { showToast } = useToast();
    const { data, error, loading, reload } = useApi(listAccounts, []);
    const [dialog, setDialog] = useState<Dialog>(null);
    const close = () => setDialog(null);
    const done = (message: string) => {
        showToast(message);
        reload();
    };

    return (
        <>
            <PageHeader
                title={t('accounts.title')}
                description={t('accounts.description')}
                actions={
                    <>
                        <Button icon='refresh' onClick={reload} busy={loading && !!data}>
                            {t('common.reload')}
                        </Button>
                        <Button variant='primary' icon='person_add' onClick={() => setDialog({ kind: 'create' })}>
                            {t('accounts.create')}
                        </Button>
                    </>
                }
            />
            <div className='flex flex-col gap-4'>
                <ErrorAlert error={error} onRetry={reload} />
                {loading && !data ? (
                    <Loading />
                ) : data && data.length === 0 ? (
                    <EmptyState icon='group' />
                ) : (
                    data && (
                        <Table>
                            <THead>
                                <Th>{t('accounts.email')}</Th>
                                <Th>{t('accounts.displayName')}</Th>
                                <Th>{t('accounts.role')}</Th>
                                <Th>{t('accounts.language')}</Th>
                                <Th>{t('accounts.state')}</Th>
                                <Th>{t('accounts.createdAt')}</Th>
                                <Th className='text-right'>{t('common.actions')}</Th>
                            </THead>
                            <TBody>
                                {data.map(a => (
                                    <Tr key={a.id}>
                                        <Td className='font-medium break-all'>
                                            {a.email}
                                            {me?.id === a.id && (
                                                <span className='ml-2 text-xs font-normal text-muted'>
                                                    {t('accounts.you')}
                                                </span>
                                            )}
                                        </Td>
                                        <Td className='break-words'>{a.display_name}</Td>
                                        <Td>
                                            <RoleBadge role={a.role} />
                                        </Td>
                                        <Td>{t(`lang.${a.language}`)}</Td>
                                        <Td>
                                            <div className='flex flex-wrap gap-1'>
                                                {a.disabled ? (
                                                    <Badge tone='danger'>{t('accounts.disabled')}</Badge>
                                                ) : (
                                                    <Badge tone='success'>{t('accounts.enabled')}</Badge>
                                                )}
                                                {a.locked_until && new Date(a.locked_until).getTime() > Date.now() && (
                                                    <Badge tone='warning' icon='lock_clock'>
                                                        {t('accounts.lockedUntil', {
                                                            date: formatDateTime(a.locked_until),
                                                        })}
                                                    </Badge>
                                                )}
                                            </div>
                                        </Td>
                                        <Td className='whitespace-nowrap'>{formatDateTime(a.created_at)}</Td>
                                        <Td className='text-right'>
                                            <div className='flex flex-wrap justify-end gap-1'>
                                                <Button
                                                    size='sm'
                                                    icon='alternate_email'
                                                    onClick={() => setDialog({ kind: 'email', account: a })}
                                                >
                                                    {t('accounts.changeEmail')}
                                                </Button>
                                                <Button
                                                    size='sm'
                                                    icon='edit'
                                                    onClick={() => setDialog({ kind: 'displayName', account: a })}
                                                >
                                                    {t('accounts.changeDisplayName')}
                                                </Button>
                                                <Button
                                                    size='sm'
                                                    icon='badge'
                                                    onClick={() => setDialog({ kind: 'role', account: a })}
                                                >
                                                    {t('accounts.changeRole')}
                                                </Button>
                                                <Button
                                                    size='sm'
                                                    icon='password'
                                                    onClick={() => setDialog({ kind: 'password', account: a })}
                                                >
                                                    {t('accounts.resetPassword')}
                                                </Button>
                                                <Button
                                                    size='sm'
                                                    icon={a.disabled ? 'check_circle' : 'do_not_disturb_on'}
                                                    onClick={() => setDialog({ kind: 'toggle', account: a })}
                                                >
                                                    {a.disabled ? t('accounts.enable') : t('accounts.disable')}
                                                </Button>
                                                <Button
                                                    size='sm'
                                                    variant='danger'
                                                    icon='delete'
                                                    onClick={() => setDialog({ kind: 'delete', account: a })}
                                                >
                                                    {t('common.delete')}
                                                </Button>
                                            </div>
                                        </Td>
                                    </Tr>
                                ))}
                            </TBody>
                        </Table>
                    )
                )}
            </div>

            <CreateAccountModal
                open={dialog?.kind === 'create'}
                onClose={close}
                onCreated={() => done(t('accounts.created'))}
            />
            <EmailModal
                account={dialog?.kind === 'email' ? dialog.account : null}
                self={dialog?.kind === 'email' && dialog.account.id === me?.id}
                onClose={close}
                onSaved={() => done(t('accounts.emailChanged'))}
            />
            <DisplayNameModal
                account={dialog?.kind === 'displayName' ? dialog.account : null}
                onClose={close}
                onSaved={() => {
                    done(t('common.saved'));
                    // The header shows the own display name.
                    if (dialog?.kind === 'displayName' && dialog.account.id === me?.id) {
                        refreshMe().catch(() => {});
                    }
                }}
            />
            <RoleModal
                account={dialog?.kind === 'role' ? dialog.account : null}
                onClose={close}
                onSaved={() => done(t('common.saved'))}
            />
            <PasswordModal
                account={dialog?.kind === 'password' ? dialog.account : null}
                onClose={close}
                onSaved={() => done(t('accounts.passwordReset'))}
            />
            <ConfirmDialog
                open={dialog?.kind === 'toggle'}
                title={
                    dialog?.kind === 'toggle' && dialog.account.disabled
                        ? t('accounts.enableTitle')
                        : t('accounts.disableTitle')
                }
                message={
                    dialog?.kind === 'toggle'
                        ? t(dialog.account.disabled ? 'accounts.enableConfirm' : 'accounts.disableConfirm', {
                              name: accountLabel(dialog.account),
                          })
                        : ''
                }
                danger={dialog?.kind === 'toggle' && !dialog.account.disabled}
                onClose={close}
                onConfirm={async () => {
                    if (dialog?.kind !== 'toggle') return;
                    await setAccountDisabled(dialog.account.id, !dialog.account.disabled);
                    done(t('common.done'));
                }}
            />
            <ConfirmDialog
                open={dialog?.kind === 'delete'}
                title={t('accounts.deleteTitle')}
                message={
                    dialog?.kind === 'delete' ? t('accounts.deleteConfirm', { name: accountLabel(dialog.account) }) : ''
                }
                confirmLabel={t('common.delete')}
                danger
                onClose={close}
                onConfirm={async () => {
                    if (dialog?.kind !== 'delete') return;
                    await deleteAccount(dialog.account.id);
                    done(t('common.deleted'));
                }}
            />
        </>
    );
}

function RoleSelect({ id, value, onChange }: { id: string; value: Role; onChange: (r: Role) => void }) {
    const { t } = useTranslation();
    return (
        <select id={id} className={inputClass} value={value} onChange={e => onChange(e.target.value as Role)}>
            <option value='viewer'>{t('role.viewer')}</option>
            <option value='admin'>{t('role.admin')}</option>
        </select>
    );
}

function FormModal({
    open,
    title,
    formId,
    busy,
    error,
    submitLabel,
    onClose,
    onSubmit,
    children,
}: {
    open: boolean;
    title: string;
    formId: string;
    busy: boolean;
    error: string | null;
    submitLabel: string;
    onClose: () => void;
    onSubmit: (e: FormEvent) => void;
    children: ReactNode;
}) {
    const { t } = useTranslation();
    return (
        <Modal
            open={open}
            title={title}
            onClose={onClose}
            locked={busy}
            footer={
                <>
                    <Button onClick={onClose} disabled={busy}>
                        {t('common.cancel')}
                    </Button>
                    <Button variant='primary' type='submit' form={formId} busy={busy}>
                        {submitLabel}
                    </Button>
                </>
            }
        >
            <form id={formId} onSubmit={onSubmit} className='flex flex-col gap-4'>
                {children}
                {error && <Alert tone='danger'>{error}</Alert>}
            </form>
        </Modal>
    );
}

/** Runs an async submit with busy and error state. */
function useSubmit() {
    const { t } = useTranslation();
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);
    async function run(fn: () => Promise<void>) {
        setBusy(true);
        setError(null);
        try {
            await fn();
        } catch (e) {
            setError(accountErrorMessage(t, e));
        } finally {
            setBusy(false);
        }
    }
    return { busy, error, setError, run };
}

function CreateAccountModal({
    open,
    onClose,
    onCreated,
}: {
    open: boolean;
    onClose: () => void;
    onCreated: () => void;
}) {
    const { t } = useTranslation();
    const [email, setEmail] = useState('');
    const [displayName, setDisplayName] = useState('');
    const [role, setRole] = useState<Role>('viewer');
    const [language, setLanguage] = useState<LangSetting>('auto');
    const [password, setPassword] = useState('');
    const { busy, error, setError, run } = useSubmit();

    useEffect(() => {
        if (!open) return;
        setEmail('');
        setDisplayName('');
        setRole('viewer');
        setLanguage('auto');
        setPassword('');
        setError(null);
    }, [open, setError]);

    return (
        <FormModal
            open={open}
            title={t('accounts.createTitle')}
            formId='create-account-form'
            busy={busy}
            error={error}
            submitLabel={t('accounts.create')}
            onClose={onClose}
            onSubmit={e => {
                e.preventDefault();
                if (password.length < MIN_PASSWORD_LENGTH) {
                    setError(t('password.tooShort', { min: MIN_PASSWORD_LENGTH }));
                    return;
                }
                void run(async () => {
                    await createAccount({
                        email: email.trim(),
                        display_name: displayName.trim(),
                        role,
                        language,
                        password,
                    });
                    onClose();
                    onCreated();
                });
            }}
        >
            <Field label={t('accounts.email')} htmlFor='ca-email' hint={t('accounts.emailHint')} required>
                <input
                    id='ca-email'
                    {...emailInputProps}
                    className={inputClass}
                    value={email}
                    required
                    autoComplete='off'
                    onChange={e => setEmail(e.target.value)}
                />
            </Field>
            <Field
                label={t('accounts.displayName')}
                htmlFor='ca-display-name'
                hint={t('accounts.displayNameHint', { max: MAX_DISPLAY_NAME_LENGTH })}
                required
            >
                <input
                    id='ca-display-name'
                    className={inputClass}
                    value={displayName}
                    required
                    autoComplete='off'
                    onChange={e => setDisplayName(e.target.value)}
                />
            </Field>
            <Field label={t('accounts.role')} htmlFor='ca-role'>
                <RoleSelect id='ca-role' value={role} onChange={setRole} />
            </Field>
            <Field label={t('accounts.language')} htmlFor='ca-language'>
                <select
                    id='ca-language'
                    className={inputClass}
                    value={language}
                    onChange={e => setLanguage(e.target.value as LangSetting)}
                >
                    {LANG_SETTINGS.map(code => (
                        <option key={code} value={code}>
                            {t(`lang.${code}`)}
                        </option>
                    ))}
                </select>
            </Field>
            <Field
                label={t('password.password')}
                htmlFor='ca-password'
                hint={t('password.minHint', { min: MIN_PASSWORD_LENGTH })}
                required
            >
                <input
                    id='ca-password'
                    type='password'
                    className={inputClass}
                    value={password}
                    required
                    autoComplete='new-password'
                    onChange={e => setPassword(e.target.value)}
                />
            </Field>
        </FormModal>
    );
}

function EmailModal({
    account,
    self,
    onClose,
    onSaved,
}: {
    account: Account | null;
    /** The account is the signed-in one: the change logs the user out. */
    self: boolean;
    onClose: () => void;
    onSaved: () => void;
}) {
    const { t } = useTranslation();
    const [email, setEmail] = useState('');
    const { busy, error, setError, run } = useSubmit();

    useEffect(() => {
        if (!account) return;
        setEmail(account.email);
        setError(null);
    }, [account, setError]);

    return (
        <FormModal
            open={account !== null}
            title={t('accounts.changeEmailTitle', { name: account ? accountLabel(account) : '' })}
            formId='email-form'
            busy={busy}
            error={error}
            submitLabel={t('common.save')}
            onClose={onClose}
            onSubmit={e => {
                e.preventDefault();
                if (!account) return;
                void run(async () => {
                    await setAccountEmail(account.id, email.trim());
                    onClose();
                    onSaved();
                });
            }}
        >
            <Field label={t('accounts.email')} htmlFor='ea-email' hint={t('accounts.emailHint')} required>
                <input
                    id='ea-email'
                    {...emailInputProps}
                    className={inputClass}
                    value={email}
                    required
                    autoComplete='off'
                    onChange={e => setEmail(e.target.value)}
                />
            </Field>
            <p className='text-xs text-muted'>{t('accounts.changeEmailNote')}</p>
            {self && <Alert tone='warning'>{t('accounts.changeOwnEmailWarning')}</Alert>}
        </FormModal>
    );
}

function DisplayNameModal({
    account,
    onClose,
    onSaved,
}: {
    account: Account | null;
    onClose: () => void;
    onSaved: () => void;
}) {
    const { t } = useTranslation();
    const [displayName, setDisplayName] = useState('');
    const { busy, error, setError, run } = useSubmit();

    useEffect(() => {
        if (!account) return;
        setDisplayName(account.display_name);
        setError(null);
    }, [account, setError]);

    return (
        <FormModal
            open={account !== null}
            title={t('accounts.changeDisplayNameTitle', { name: account ? accountLabel(account) : '' })}
            formId='display-name-form'
            busy={busy}
            error={error}
            submitLabel={t('common.save')}
            onClose={onClose}
            onSubmit={e => {
                e.preventDefault();
                if (!account) return;
                void run(async () => {
                    await setAccountDisplayName(account.id, displayName.trim());
                    onClose();
                    onSaved();
                });
            }}
        >
            <Field
                label={t('accounts.displayName')}
                htmlFor='da-display-name'
                hint={t('accounts.displayNameHint', { max: MAX_DISPLAY_NAME_LENGTH })}
                required
            >
                <input
                    id='da-display-name'
                    className={inputClass}
                    value={displayName}
                    required
                    autoComplete='off'
                    onChange={e => setDisplayName(e.target.value)}
                />
            </Field>
        </FormModal>
    );
}

function RoleModal({
    account,
    onClose,
    onSaved,
}: {
    account: Account | null;
    onClose: () => void;
    onSaved: () => void;
}) {
    const { t } = useTranslation();
    const [role, setRole] = useState<Role>('viewer');
    const { busy, error, setError, run } = useSubmit();

    useEffect(() => {
        if (!account) return;
        setRole(account.role);
        setError(null);
    }, [account, setError]);

    return (
        <FormModal
            open={account !== null}
            title={t('accounts.changeRoleTitle', { name: account ? accountLabel(account) : '' })}
            formId='role-form'
            busy={busy}
            error={error}
            submitLabel={t('common.save')}
            onClose={onClose}
            onSubmit={e => {
                e.preventDefault();
                if (!account) return;
                void run(async () => {
                    await setAccountRole(account.id, role);
                    onClose();
                    onSaved();
                });
            }}
        >
            <Field label={t('accounts.role')} htmlFor='ra-role'>
                <RoleSelect id='ra-role' value={role} onChange={setRole} />
            </Field>
        </FormModal>
    );
}

function PasswordModal({
    account,
    onClose,
    onSaved,
}: {
    account: Account | null;
    onClose: () => void;
    onSaved: () => void;
}) {
    const { t } = useTranslation();
    const [password, setPassword] = useState('');
    const [confirm, setConfirm] = useState('');
    const { busy, error, setError, run } = useSubmit();

    useEffect(() => {
        if (!account) return;
        setPassword('');
        setConfirm('');
        setError(null);
    }, [account, setError]);

    return (
        <FormModal
            open={account !== null}
            title={t('accounts.resetPasswordTitle', { name: account ? accountLabel(account) : '' })}
            formId='password-form'
            busy={busy}
            error={error}
            submitLabel={t('accounts.resetPassword')}
            onClose={onClose}
            onSubmit={e => {
                e.preventDefault();
                if (!account) return;
                if (password.length < MIN_PASSWORD_LENGTH) {
                    setError(t('password.tooShort', { min: MIN_PASSWORD_LENGTH }));
                    return;
                }
                if (password !== confirm) {
                    setError(t('password.mismatch'));
                    return;
                }
                void run(async () => {
                    await resetAccountPassword(account.id, password);
                    onClose();
                    onSaved();
                });
            }}
        >
            <Field
                label={t('password.new')}
                htmlFor='rp-password'
                hint={t('password.minHint', { min: MIN_PASSWORD_LENGTH })}
                required
            >
                <input
                    id='rp-password'
                    type='password'
                    className={inputClass}
                    value={password}
                    required
                    autoComplete='new-password'
                    onChange={e => setPassword(e.target.value)}
                />
            </Field>
            <Field label={t('password.confirm')} htmlFor='rp-confirm' required>
                <input
                    id='rp-confirm'
                    type='password'
                    className={inputClass}
                    value={confirm}
                    required
                    autoComplete='new-password'
                    onChange={e => setConfirm(e.target.value)}
                />
            </Field>
        </FormModal>
    );
}
