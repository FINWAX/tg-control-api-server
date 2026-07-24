import './globals.css';
import type { ReactNode } from 'react';

export const metadata = {
  title: 'TgApi Console',
  description: 'Management console for the TgApi service',
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="ru">
      <body>{children}</body>
    </html>
  );
}
