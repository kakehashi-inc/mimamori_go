import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import {
    createAccount,
    deleteAccount,
    listAccounts,
    resetAccountPassword,
    setAccountDisabled,
    setAccountRole,
} from '../api/client';
import { Badge } from '../components/Badge';
import { LANG_SETTINGS, MIN_PASSWORD_LENGTH, type LangSetting } from '../constants';
import { Button } from '../components/Button';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { Field, inputClass } from '../components/FormControls';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { Alert, EmptyState, ErrorAlert, Loading } from '../components/StateViews';
import { RoleBadge } from '../components/StatusBadges';
import { Table, TBody, Td, Th, THead, Tr } from '../components/Table';
import { useAuth } from '../context/AuthContext';
import { useToast } from '../context/ToastContext';
import { useApi } from '../hooks/useApi';
import type { Account, Role } from '../types';
import { errorMessage, formatDateTime } from '../utils/format';

type Dialog =
    | { kind: 'create' }
    | { kind: 'role'; account: Account }
    | { kind: 'password'; account: Account }
    | { kind: 'toggle'; account: Account }
    | { kind: 'delete'; account: Account }
    | null;

export function AccountsPage() {
    const { t } = useTranslation();
    const { me } = useAuth();
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
                                <Th>{t('accounts.name')}</Th>
                                <Th>{t('accounts.role')}</Th>
                                <Th>{t('accounts.language')}</Th>
                                <Th>{t('accounts.state')}</Th>
                                <Th>{t('accounts.createdAt')}</Th>
                                <Th className='text-right'>{t('common.actions')}</Th>
                            </THead>
                            <TBody>
                                {data.map(a => (
                                    <Tr key={a.id}>
                                        <Td className='font-medium'>
                                            {a.name}
                                            {me?.id === a.id && (
                                                <span className='ml-2 text-xs text-muted'>{t('accounts.you')}</span>
                                            )}
                                        </Td>
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
                              name: dialog.account.name,
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
                message={dialog?.kind === 'delete' ? t('accounts.deleteConfirm', { name: dialog.account.name }) : ''}
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
            setError(errorMessage(t, e));
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
    const [name, setName] = useState('');
    const [role, setRole] = useState<Role>('viewer');
    const [language, setLanguage] = useState<LangSetting>('auto');
    const [password, setPassword] = useState('');
    const { busy, error, setError, run } = useSubmit();

    useEffect(() => {
        if (!open) return;
        setName('');
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
                    await createAccount({ name: name.trim(), role, language, password });
                    onClose();
                    onCreated();
                });
            }}
        >
            <Field label={t('accounts.name')} htmlFor='ca-name' required>
                <input
                    id='ca-name'
                    className={inputClass}
                    value={name}
                    required
                    autoComplete='off'
                    onChange={e => setName(e.target.value)}
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
            title={t('accounts.changeRoleTitle', { name: account?.name ?? '' })}
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
            title={t('accounts.resetPasswordTitle', { name: account?.name ?? '' })}
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
