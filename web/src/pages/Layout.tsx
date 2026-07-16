import { NavLink, Outlet } from 'react-router-dom';
import { useAuth } from '../components/auth';

interface NavItem {
  to: string;
  label: string;
  adminOnly?: boolean;
}

const NAV_ITEMS: NavItem[] = [
  { to: '/', label: 'Dashboard' },
  { to: '/upstreams', label: 'Upstreams' },
  { to: '/events', label: 'Events' },
  { to: '/users', label: 'Users', adminOnly: true },
  { to: '/config', label: 'Config', adminOnly: true },
];

export function Layout() {
  const { user, isAdmin, logout } = useAuth();

  const visibleItems = NAV_ITEMS.filter((item) => !item.adminOnly || isAdmin);

  return (
    <div className="flex h-full flex-col bg-slate-950">
      {/* Top nav bar */}
      <header className="flex h-11 shrink-0 items-center border-b border-slate-800 px-4 gap-6">
        {/* Brand */}
        <div className="flex items-center gap-2 mr-2">
          <span className="grid h-6 w-6 place-items-center rounded bg-brand text-brand-fg text-xs font-bold">
            S
          </span>
          <span className="text-slate-100 font-semibold text-sm tracking-tight">Specula</span>
        </div>

        {/* Nav links */}
        <nav className="flex items-center gap-1 flex-1">
          {visibleItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === '/'}
              className={({ isActive }) =>
                `px-2.5 py-1 rounded-md text-[13px] transition-colors ${
                  isActive
                    ? 'text-slate-100 bg-slate-800'
                    : 'text-slate-400 hover:text-slate-200 hover:bg-slate-800/60'
                }`
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        {/* User info + logout */}
        <div className="flex items-center gap-3 text-xs text-slate-400">
          <span className="hidden sm:block truncate max-w-[160px]">{user?.email}</span>
          {isAdmin && (
            <span className="border border-brand/40 text-brand px-1.5 py-0.5 rounded text-[10px] font-semibold uppercase tracking-wider">
              admin
            </span>
          )}
          <button
            onClick={() => void logout()}
            className="text-slate-500 hover:text-slate-200 transition-colors"
          >
            Sign out
          </button>
        </div>
      </header>

      {/* Page content */}
      <main className="flex-1 overflow-y-auto p-5">
        <Outlet />
      </main>
    </div>
  );
}
