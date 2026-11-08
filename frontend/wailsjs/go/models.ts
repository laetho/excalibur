export namespace main {
	
	export class AIChatMessage {
	    role: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new AIChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	    }
	}
	export class AISettings {
	    enabled: boolean;
	    baseUrl: string;
	    model: string;
	    hasApiKey: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AISettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.baseUrl = source["baseUrl"];
	        this.model = source["model"];
	        this.hasApiKey = source["hasApiKey"];
	    }
	}
	export class AISettingsUpdate {
	    enabled: boolean;
	    baseUrl: string;
	    model: string;
	    apiKey: string;
	    clearApiKey: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AISettingsUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.baseUrl = source["baseUrl"];
	        this.model = source["model"];
	        this.apiKey = source["apiKey"];
	        this.clearApiKey = source["clearApiKey"];
	    }
	}
	export class BoardInfo {
	    id: string;
	    name: string;
	    modified: number;
	    isLink: boolean;
	    target?: string;
	    texts: string[];
	    thumbnail?: string;
	
	    static createFrom(source: any = {}) {
	        return new BoardInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.modified = source["modified"];
	        this.isLink = source["isLink"];
	        this.target = source["target"];
	        this.texts = source["texts"];
	        this.thumbnail = source["thumbnail"];
	    }
	}
	export class GroupInfo {
	    id: string;
	    name: string;
	    kind: string;
	
	    static createFrom(source: any = {}) {
	        return new GroupInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	    }
	}
	export class NatsServerConfig {
	    id: string;
	    name: string;
	    url: string;
	    username?: string;
	    password?: string;
	    buckets: string[];
	
	    static createFrom(source: any = {}) {
	        return new NatsServerConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.url = source["url"];
	        this.username = source["username"];
	        this.password = source["password"];
	        this.buckets = source["buckets"];
	    }
	}
	export class Notice {
	    id: string;
	    title: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new Notice(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.message = source["message"];
	    }
	}
	export class PickFileResult {
	    name: string;
	    dataUrl: string;
	
	    static createFrom(source: any = {}) {
	        return new PickFileResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.dataUrl = source["dataUrl"];
	    }
	}
	export class SourceInfo {
	    id: string;
	    name: string;
	    kind: string;
	    groups: GroupInfo[];
	
	    static createFrom(source: any = {}) {
	        return new SourceInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.groups = this.convertValues(source["groups"], GroupInfo);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class StoreSnapshot {
	    folder: string;
	    boards: BoardInfo[];
	    selection?: string;
	    sortOrder: string;
	    showThumbnails: boolean;
	    canvasAppearance: string;
	    alerts: Notice[];
	    isAlertPresented: boolean;
	    sources: SourceInfo[];
	    activeSource: string;
	    activeGroup: string;
	    ai: AISettings;
	
	    static createFrom(source: any = {}) {
	        return new StoreSnapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.folder = source["folder"];
	        this.boards = this.convertValues(source["boards"], BoardInfo);
	        this.selection = source["selection"];
	        this.sortOrder = source["sortOrder"];
	        this.showThumbnails = source["showThumbnails"];
	        this.canvasAppearance = source["canvasAppearance"];
	        this.alerts = this.convertValues(source["alerts"], Notice);
	        this.isAlertPresented = source["isAlertPresented"];
	        this.sources = this.convertValues(source["sources"], SourceInfo);
	        this.activeSource = source["activeSource"];
	        this.activeGroup = source["activeGroup"];
	        this.ai = this.convertValues(source["ai"], AISettings);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

