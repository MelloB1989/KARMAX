import * as Contacts from 'expo-contacts/legacy';

async function ensurePermission(): Promise<boolean> {
  const { status } = await Contacts.requestPermissionsAsync();
  return status === 'granted';
}

// normalize keeps only digits so loosely-formatted numbers still match.
function normalize(p: string): string {
  return (p || '').replace(/[^\d]/g, '');
}

// lookupNameByPhone resolves a phone number (e.g. a WhatsApp number) to the
// saved contact's display name, or null if there's no match / no permission.
export async function lookupNameByPhone(phone: string): Promise<string | null> {
  if (!(await ensurePermission())) return null;
  const target = normalize(phone);
  if (target.length < 6) return null;

  const { data } = await Contacts.getContactsAsync({
    fields: [Contacts.Fields.PhoneNumbers, Contacts.Fields.Name],
  });
  for (const c of data) {
    for (const pn of c.phoneNumbers ?? []) {
      const num = normalize(pn.number ?? pn.digits ?? '');
      if (num && (num === target || num.endsWith(target) || target.endsWith(num))) {
        return c.name ?? null;
      }
    }
  }
  return null;
}

// createContact adds a new contact (name + phone) and returns its system id.
export async function createContact(name: string, phone: string): Promise<string> {
  if (!(await ensurePermission())) throw new Error('contacts permission denied');
  const parts = name.trim().split(/\s+/);
  const firstName = parts.shift() || name;
  const lastName = parts.join(' ');

  const contact = {
    name,
    firstName,
    lastName,
    contactType: Contacts.ContactTypes.Person,
    phoneNumbers: [{ label: 'mobile', number: phone }],
  } as unknown as Contacts.Contact;

  return Contacts.addContactAsync(contact);
}

// getAllContacts returns the phone's directory as {name, phones[]} for syncing
// to KARMAX (so it can resolve WhatsApp numbers to saved names).
export async function getAllContacts(): Promise<{ name: string; phones: string[] }[]> {
  if (!(await ensurePermission())) return [];
  const { data } = await Contacts.getContactsAsync({
    fields: [Contacts.Fields.Name, Contacts.Fields.PhoneNumbers],
  });
  const out: { name: string; phones: string[] }[] = [];
  for (const c of data) {
    const name = (c.name ?? '').trim();
    const phones = (c.phoneNumbers ?? [])
      .map((pn) => (pn.number ?? pn.digits ?? '').trim())
      .filter(Boolean);
    if (name && phones.length) out.push({ name, phones });
  }
  return out;
}
