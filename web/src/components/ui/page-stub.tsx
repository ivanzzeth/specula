import { useTranslation } from 'react-i18next';

import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';

/**
 * PageStub — a placeholder for a route that a UI agent has not built yet.
 *
 * It exists so the router, the nav and the build are complete from day one and
 * the four parallel agents can each fill in their own files without touching
 * App.tsx or Layout.tsx. Replace the whole file when implementing the page.
 *
 * `title`/`brief`/`owner`/`endpoints` are passed in by the caller and are
 * developer-facing scaffolding copy, so they are rendered verbatim; only the
 * chrome this component owns is translated.
 */
export function PageStub({
  title,
  brief,
  owner,
  endpoints,
}: {
  title: string;
  /** What this page must do, per REGISTRY-DESIGN. */
  brief: string;
  /** Which UI agent owns it. */
  owner: string;
  /** The client functions it should call. */
  endpoints: string[];
}) {
  const { t } = useTranslation();

  return (
    <div className="space-y-3">
      <div>
        <h1 className="text-display font-semibold text-slate-100">{title}</h1>
        <p className="mt-0.5 text-data text-slate-400">{brief}</p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>{t('stub.title')}</CardTitle>
          <span className="label-caps text-micro text-slate-500">{owner}</span>
        </CardHeader>
        <CardContent className="space-y-2">
          <p className="text-data text-slate-400">{t('stub.body')}</p>
          <div className="space-y-1">
            <span className="section-label">{t('stub.contract')}</span>
            <ul className="space-y-0.5">
              {endpoints.map((e) => (
                <li key={e} className="text-data text-slate-300">
                  {e}
                </li>
              ))}
            </ul>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
