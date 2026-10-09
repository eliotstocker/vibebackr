export type AccessPolicy = {
    visibility: 'private' | 'public' | 'shared';
    shares?: string[];
};

export type User = {
    id: string;
    tenant_id?: string | null;
    email: string;
    name: string;
    role: 'admin' | 'manager' | 'member';
    manager_email?: string;
    token_version?: string;
    created_at?: string;
};

export type TenantMembership = {
    user_id: string;
    tenant_id: string;
    role: 'admin' | 'manager' | 'member';
    manager_email?: string;
    is_owner: boolean;
    created_at: string;
};

export type Collection = {
    id: string;
    tenant_id: string;
    name: string;
    slug?: string;
    default_visibility: 'private' | 'public' | 'shared';
    allowed_domains?: string[];
    vector_search_enabled?: boolean;
    auto_invite_shares?: boolean;
    origin_urls?: string[];
    created_at: string;
};

export type VibeRecord<T = Record<string, any>> = {
    id: string;
    collection_id: string;
    title: string;
    content: string;
    data: string; //json string payload
    created_by: string;
    created_at: string;
    updated_at: string;
    access_policy: AccessPolicy;
};

export type SearchResult<T = Record<string, any>> = {
    record: VibeRecord<T>;
    score: number;
    method?: string;
};

export type AuthResult = {
    access_token: string;
    refresh_token?: string;
    token_type: string;
    expires_in: number;
    user: User;
};

export type VibeClientOptions = {
    baseUrl?: string;
    collectionId?: string;
    tokenKey?: string;
    refreshTokenKey?: string;
    useRelay?: boolean;
    relayUrl?: string;
};

export type RecordCreatePayload<T = Record<string, any>> = {
    title: string;
    content?: string;
    data?: T | string;
    visibility?: 'private' | 'public' | 'shared';
    shares?: string[];
    access_policy?: AccessPolicy;
};

export type RecordUpdatePayload<T = Record<string, any>> = {
    title?: string;
    content?: string;
    data?: T | string;
    visibility?: 'private' | 'public' | 'shared';
    shares?: string[];
    access_policy?: AccessPolicy;
};

export type CollectionCreatePayload = {
    name: string;
    slug?: string;
    default_visibility?: 'private' | 'public' | 'shared';
    allowed_domains?: string[];
    origin_urls?: string[];
    vector_search_enabled?: boolean;
    auto_invite_shares?: boolean;
    records?: Array<{
        title: string;
        content?: string;
        data?: any;
        visibility?: 'private' | 'public' | 'shared';
        shares?: string[];
        access_policy?: AccessPolicy;
    }>;
};

export type VibeClient = {
    readonly baseUrl: string;
    readonly collectionId: string | null;

    getToken(): string | null;
    setToken(token: string | null, refreshToken?: string | null): void;
    getRefreshToken(): string | null;
    setRefreshToken(token: string | null): void;
    refreshToken(): Promise<AuthResult>;
    initRelay(): Promise<Window>;

    login(provider?: string, collectionId?: string): Promise<AuthResult>;
    loginWithGoogle(): Promise<AuthResult>;
    loginWithGitHub(): Promise<AuthResult>;
    loginMock(): Promise<AuthResult>;
    getLoginUrl(provider?: string, redirectUri?: string, collectionId?: string): string;
    handleAuthCallback(): string | null;
    logout(): Promise<{message: string}>;
    revokeAllSessions(): Promise<{message: string; token_version: string}>;
    listProviders(): Promise<string[]>;

    listTenants(): Promise<TenantMembership[]>;
    createTenant(): Promise<{tenant_id: string; access_token?: string; user: User}>;
    leaveTenant(tenantId: string): Promise<{message: string; tenant_id: string}>;

    listCollections(): Promise<Collection[]>;
    getCollections(): Promise<Collection[]>;
    createCollection(nameOrConfig: string | CollectionCreatePayload, defaultVisibility?: 'private' | 'public' | 'shared', originUrls?: string[]): Promise<Collection>;
    getCollection(colId?: string): Promise<Collection>;
    updateCollection(colId: string, payload: Partial<Collection>): Promise<Collection>;
    publishApp(colId: string, html: string, slug?: string): Promise<{collection_id: string; slug: string; app_url: string; origin_urls: string[]; message: string}>;
    publishApp(html: string): Promise<{collection_id: string; slug: string; app_url: string; origin_urls: string[]; message: string}>;
    deleteCollection(colId: string): Promise<{message: string; id: string}>;

    listRecords<T = Record<string, any>>(colId?: string): Promise<VibeRecord<T>[]>;
    getRecords<T = Record<string, any>>(colId?: string): Promise<VibeRecord<T>[]>;
    getRecord<T = Record<string, any>>(colIdOrRecordId: string, recordId?: string): Promise<VibeRecord<T>>;
    createRecord<T = Record<string, any>>(colId: string, payload: RecordCreatePayload<T>): Promise<VibeRecord<T>>;
    createRecord<T = Record<string, any>>(payload: RecordCreatePayload<T>, colId?: string): Promise<VibeRecord<T>>;
    updateRecord<T = Record<string, any>>(colId: string, recordId: string, payload: RecordUpdatePayload<T>): Promise<VibeRecord<T>>;
    updateRecord<T = Record<string, any>>(recordId: string, payload: RecordUpdatePayload<T>, colId?: string): Promise<VibeRecord<T>>;
    deleteRecord(colIdOrRecordId: string, recordId?: string): Promise<{message: string}>;

    searchRecords<T = Record<string, any>>(colId: string, query: string, limit?: number): Promise<SearchResult<T>[]>;
    searchRecords<T = Record<string, any>>(query: string, limit?: number): Promise<SearchResult<T>[]>;
    search<T = Record<string, any>>(colId: string, query: string, limit?: number): Promise<SearchResult<T>[]>;
    search<T = Record<string, any>>(query: string, limit?: number): Promise<SearchResult<T>[]>;
};

export declare function createVibeClient(baseUrlOrOptions?: string | VibeClientOptions, options?: VibeClientOptions): VibeClient;

export type VibeBackrStatic = {
    new (baseUrlOrOptions?: string | VibeClientOptions, options?: VibeClientOptions): VibeClient;
    (baseUrlOrOptions?: string | VibeClientOptions, options?: VibeClientOptions): VibeClient;
    createClient: typeof createVibeClient;
    createVibeClient: typeof createVibeClient;
    VibeBackr: VibeBackrStatic;
};

export declare const VibeBackr: VibeBackrStatic;

declare global {
    interface Window {
        VibeBackr: VibeBackrStatic;
        createVibeClient: typeof createVibeClient;
    }
}

export default VibeBackr;
