import { useEffect, useId, useRef, useState } from 'react';

export default function AuthForm({ auth, autoFocus = false, submitLabel = 'Continue' }) {
  const [key, setKey] = useState('');
  const [error, setError] = useState('');
  const [pending, setPending] = useState(false);
  const inputRef = useRef(null);
  const errorId = useId();

  useEffect(() => {
    if (autoFocus) inputRef.current?.focus();
  }, [autoFocus]);

  const submit = async event => {
    event.preventDefault();
    if (pending) return;
    setError('');
    setPending(true);
    try {
      await auth.signIn(key);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure));
      setPending(false);
      inputRef.current?.focus();
    }
  };

  return (
    <form className="lp-auth-form" onSubmit={submit} noValidate>
      <label className="lp-auth-label">
        <span>NeoChat API key</span>
        <input
          ref={inputRef}
          className="lp-auth-input"
          type="password"
          autoComplete="off"
          spellCheck={false}
          placeholder="Paste API key"
          value={key}
          onChange={event => {
            setKey(event.target.value);
            if (error) setError('');
          }}
          aria-invalid={Boolean(error)}
          aria-describedby={error ? errorId : undefined}
        />
      </label>
      <button className="lp-auth-submit" type="submit" disabled={pending || !key.trim()}>
        {pending ? 'Checking…' : submitLabel}
      </button>
      <p className="lp-auth-error" id={errorId} role="alert" hidden={!error}>
        {error}
      </p>
    </form>
  );
}
