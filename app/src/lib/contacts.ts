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

// upsertContactName sets the display name for a phone number: if a contact with
// that number already exists it renames it (updateContactAsync), otherwise it
// creates a new one. Returns the contact's system id. This is what backs
// KARMAX naming/renaming a raw WhatsApp number.
export async function upsertContactName(name: string, phone: string): Promise<string> {
  if (!(await ensurePermission())) throw new Error('contacts permission denied');
  const target = normalize(phone);
  if (target.length < 6) throw new Error('invalid phone number');

  const { data } = await Contacts.getContactsAsync({
    fields: [Contacts.Fields.PhoneNumbers, Contacts.Fields.Name],
  });
  const match = data.find((c) =>
    (c.phoneNumbers ?? []).some((pn) => {
      const num = normalize(pn.number ?? pn.digits ?? '');
      return num && (num === target || num.endsWith(target) || target.endsWith(num));
    }),
  );

  const parts = name.trim().split(/\s+/);
  const firstName = parts.shift() || name;
  const lastName = parts.join(' ');

  if (match?.id) {
    // updateContactAsync needs the id on the payload; keep the existing number.
    const updated = { ...match, name, firstName, lastName } as unknown as Contacts.Contact;
    await Contacts.updateContactAsync(updated);
    return match.id;
  }
  return createContact(name, phone);
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
